# Exercise 31: Channel-Based Resource Pool with Health Checks

Every database-backed application needs connection pooling: opening a fresh
connection per request is too slow, and keeping unlimited connections open
wastes the server. A pool has to manage a fixed set -- hand connections out,
take them back, and make sure they are still alive. The usual
mutex-and-condition-variable design is a minefield of forgotten unlocks and
broadcast storms. This exercise builds the pool the channel way instead: one
manager goroutine owns every connection, and all acquire, release,
health-check, and shutdown traffic flows through channels, so there is no mutex
and no data race by construction.

## What you'll build

```text
31-channel-resource-pool/
  main.go        channel-managed connection pool: waiter queue, health-check
                 ticker, failure replacement (each step a runnable program)
```

- Build: a channel-managed connection pool with a waiter queue, health checks, and failure replacement.
- Implement: `poolManager` and its variants driven by a `select` loop over acquire, release, health-tick, bad-conn, and done, plus `AcquireRequest` reply channels and `BadConnReport`.
- Verify: `go run main.go`, and `go run -race main.go` for the concurrent steps.

### Why one goroutine owning the state beats a mutex

The traditional approach uses a mutex-protected list of connections with condition variables for waiters. This works but is error-prone: forgotten unlocks, deadlocks under complex conditions, condition variable broadcast storms.

A channel-based pool uses a single manager goroutine that owns all connections. Callers request a connection by sending on an acquire channel and waiting on a per-request reply channel. They return connections on a release channel. A ticker triggers periodic health checks on idle connections. The manager evicts dead connections and creates replacements. All state lives in one goroutine, all communication is through channels. No mutex, no condition variable, no data races by construction.

## Step 1 -- Basic Acquire and Release

Build the core pool: a manager goroutine that holds N connections. Callers acquire by sending a request, receive a connection on their reply channel, and release when done.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const poolSize = 3

var connIDCounter atomic.Int64

// Conn simulates a database connection.
type Conn struct {
	ID        int64
	CreatedAt time.Time
}

// NewConn creates a simulated connection.
func NewConn() *Conn {
	id := connIDCounter.Add(1)
	return &Conn{ID: id, CreatedAt: time.Now()}
}

// AcquireRequest asks the pool for a connection.
type AcquireRequest struct {
	Reply chan *Conn
}

// NewAcquireRequest creates a request with an initialized reply channel.
func NewAcquireRequest() AcquireRequest {
	return AcquireRequest{Reply: make(chan *Conn, 1)}
}

// poolManager owns all connections and handles acquire/release.
func poolManager(acquire <-chan AcquireRequest, release <-chan *Conn, done <-chan struct{}) {
	idle := make([]*Conn, 0, poolSize)
	for i := 0; i < poolSize; i++ {
		idle = append(idle, NewConn())
	}
	fmt.Printf("[pool] initialized with %d connections\n", len(idle))

	for {
		select {
		case <-done:
			fmt.Printf("[pool] shutting down, %d idle connections\n", len(idle))
			return

		case req := <-acquire:
			if len(idle) > 0 {
				conn := idle[len(idle)-1]
				idle = idle[:len(idle)-1]
				req.Reply <- conn
			} else {
				req.Reply <- nil
			}

		case conn := <-release:
			idle = append(idle, conn)
		}
	}
}

func main() {
	acquire := make(chan AcquireRequest, 10)
	release := make(chan *Conn, 10)
	done := make(chan struct{})

	go poolManager(acquire, release, done)

	var wg sync.WaitGroup
	for i := 1; i <= 5; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			req := NewAcquireRequest()
			acquire <- req
			conn := <-req.Reply
			if conn == nil {
				fmt.Printf("  worker %d: no connection available\n", workerID)
				return
			}
			fmt.Printf("  worker %d: acquired conn %d\n", workerID, conn.ID)
			time.Sleep(50 * time.Millisecond)
			release <- conn
			fmt.Printf("  worker %d: released conn %d\n", workerID, conn.ID)
		}(i)
	}

	wg.Wait()
	close(done)
}
```

Key observations:
- The pool starts with 3 connections. Workers 1-3 get a connection, workers 4-5 get `nil` (pool exhausted)
- The idle slice acts as a LIFO stack -- most recently released connections are reused first (better for cache locality)
- The manager goroutine is the sole owner of the idle list -- no concurrent access

### Verification
```bash
go run main.go
```
Expected output (order may vary):
```
[pool] initialized with 3 connections
  worker 1: acquired conn 3
  worker 2: acquired conn 2
  worker 3: acquired conn 1
  worker 4: no connection available
  worker 5: no connection available
  worker 1: released conn 3
  worker 2: released conn 2
  worker 3: released conn 1
[pool] shutting down, 3 idle connections
```

## Step 2 -- Waiter Queue for Pool Exhaustion

Instead of returning nil when the pool is exhausted, queue the caller and serve it when a connection is released.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const waiterPoolSize = 3

var waiterConnID atomic.Int64

type Conn struct {
	ID        int64
	CreatedAt time.Time
}

func NewConn() *Conn {
	return &Conn{ID: waiterConnID.Add(1), CreatedAt: time.Now()}
}

type AcquireRequest struct {
	Reply chan *Conn
}

func NewAcquireRequest() AcquireRequest {
	return AcquireRequest{Reply: make(chan *Conn, 1)}
}

// poolManagerWithWaiters queues callers when no connections are available.
func poolManagerWithWaiters(acquire <-chan AcquireRequest, release <-chan *Conn, done <-chan struct{}) {
	idle := make([]*Conn, 0, waiterPoolSize)
	for i := 0; i < waiterPoolSize; i++ {
		idle = append(idle, NewConn())
	}

	waiters := make([]AcquireRequest, 0)

	for {
		select {
		case <-done:
			// Unblock any remaining waiters with nil.
			for _, w := range waiters {
				w.Reply <- nil
			}
			return

		case req := <-acquire:
			if len(idle) > 0 {
				conn := idle[len(idle)-1]
				idle = idle[:len(idle)-1]
				req.Reply <- conn
			} else {
				waiters = append(waiters, req)
				fmt.Printf("[pool] no idle conns, queued waiter (queue: %d)\n", len(waiters))
			}

		case conn := <-release:
			if len(waiters) > 0 {
				next := waiters[0]
				waiters = waiters[1:]
				next.Reply <- conn
				fmt.Printf("[pool] served queued waiter (queue: %d)\n", len(waiters))
			} else {
				idle = append(idle, conn)
			}
		}
	}
}

func main() {
	acquire := make(chan AcquireRequest, 10)
	release := make(chan *Conn, 10)
	done := make(chan struct{})

	go poolManagerWithWaiters(acquire, release, done)

	var wg sync.WaitGroup
	epoch := time.Now()

	for i := 1; i <= 6; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			start := time.Now()
			req := NewAcquireRequest()
			acquire <- req
			conn := <-req.Reply
			if conn == nil {
				fmt.Printf("  worker %d: pool shut down\n", workerID)
				return
			}
			waitTime := time.Since(start).Round(time.Millisecond)
			fmt.Printf("  worker %d: acquired conn %d (waited %v)\n",
				workerID, conn.ID, waitTime)
			time.Sleep(80 * time.Millisecond)
			release <- conn
			fmt.Printf("  worker %d: released conn %d\n", workerID, conn.ID)
		}(i)
	}

	wg.Wait()
	close(done)
	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("\ntotal time: %v (pool size: %d, workers: 6)\n", elapsed, waiterPoolSize)
}
```

With 6 workers and 3 connections:
- Workers 1-3 acquire immediately (wait ~0ms)
- Workers 4-6 are queued and served as connections are released (wait ~80ms)
- Total time is ~160ms (two rounds of 3 workers), not ~480ms (6 sequential)

### Verification
```bash
go run -race main.go
```
Expected output (approximate):
```
[pool] no idle conns, queued waiter (queue: 1)
[pool] no idle conns, queued waiter (queue: 2)
[pool] no idle conns, queued waiter (queue: 3)
  worker 1: acquired conn 3 (waited 0s)
  worker 2: acquired conn 2 (waited 0s)
  worker 3: acquired conn 1 (waited 0s)
  worker 1: released conn 3
[pool] served queued waiter (queue: 2)
  worker 4: acquired conn 3 (waited 80ms)
  worker 2: released conn 2
[pool] served queued waiter (queue: 1)
  worker 5: acquired conn 2 (waited 80ms)
  worker 3: released conn 1
[pool] served queued waiter (queue: 0)
  worker 6: acquired conn 1 (waited 80ms)
  worker 4: released conn 3
  worker 5: released conn 2
  worker 6: released conn 1

total time: 160ms (pool size: 3, workers: 6)
```

## Step 3 -- Health-Check Ticker for Idle Connections

Add a periodic health check that validates idle connections. Dead connections are evicted and replaced with fresh ones.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	healthPoolSize    = 4
	healthCheckPeriod = 100 * time.Millisecond
	connMaxAge        = 250 * time.Millisecond
)

var healthConnID atomic.Int64

type Conn struct {
	ID        int64
	CreatedAt time.Time
	healthy   bool
}

func NewConn() *Conn {
	return &Conn{
		ID:        healthConnID.Add(1),
		CreatedAt: time.Now(),
		healthy:   true,
	}
}

// IsHealthy checks if the connection is still usable.
// Simulates failure: connections older than connMaxAge are considered dead.
func (c *Conn) IsHealthy() bool {
	return c.healthy && time.Since(c.CreatedAt) < connMaxAge
}

type AcquireRequest struct {
	Reply chan *Conn
}

func NewAcquireRequest() AcquireRequest {
	return AcquireRequest{Reply: make(chan *Conn, 1)}
}

// healthCheckPool runs health checks on idle connections when the ticker fires.
func healthCheckPool(
	acquire <-chan AcquireRequest,
	release <-chan *Conn,
	done <-chan struct{},
	healthTick <-chan time.Time,
) {
	idle := make([]*Conn, 0, healthPoolSize)
	for i := 0; i < healthPoolSize; i++ {
		idle = append(idle, NewConn())
	}
	fmt.Printf("[pool] initialized: %d connections\n", healthPoolSize)

	totalCreated := healthPoolSize
	totalEvicted := 0
	waiters := make([]AcquireRequest, 0)

	for {
		select {
		case <-done:
			for _, w := range waiters {
				w.Reply <- nil
			}
			fmt.Printf("[pool] shutdown: created=%d, evicted=%d\n", totalCreated, totalEvicted)
			return

		case req := <-acquire:
			if len(idle) > 0 {
				conn := idle[len(idle)-1]
				idle = idle[:len(idle)-1]
				req.Reply <- conn
			} else {
				waiters = append(waiters, req)
			}

		case conn := <-release:
			if len(waiters) > 0 {
				next := waiters[0]
				waiters = waiters[1:]
				next.Reply <- conn
			} else {
				idle = append(idle, conn)
			}

		case <-healthTick:
			healthy := make([]*Conn, 0, len(idle))
			evicted := 0
			for _, conn := range idle {
				if conn.IsHealthy() {
					healthy = append(healthy, conn)
				} else {
					evicted++
					totalEvicted++
					fmt.Printf("[health] evicted conn %d (age: %v)\n",
						conn.ID, time.Since(conn.CreatedAt).Round(time.Millisecond))
				}
			}
			// Replace evicted connections.
			for i := 0; i < evicted; i++ {
				replacement := NewConn()
				totalCreated++
				healthy = append(healthy, replacement)
				fmt.Printf("[health] created replacement conn %d\n", replacement.ID)
			}
			idle = healthy
			fmt.Printf("[health] check complete: %d idle, %d evicted\n", len(idle), evicted)
		}
	}
}

func main() {
	acquire := make(chan AcquireRequest, 10)
	release := make(chan *Conn, 10)
	done := make(chan struct{})
	ticker := time.NewTicker(healthCheckPeriod)
	defer ticker.Stop()

	go healthCheckPool(acquire, release, done, ticker.C)

	var wg sync.WaitGroup

	// Phase 1: use connections normally.
	fmt.Println("=== Phase 1: normal usage ===")
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			req := NewAcquireRequest()
			acquire <- req
			conn := <-req.Reply
			if conn == nil {
				return
			}
			fmt.Printf("  worker %d: using conn %d\n", id, conn.ID)
			time.Sleep(40 * time.Millisecond)
			release <- conn
		}(i)
	}
	wg.Wait()

	// Phase 2: wait for health checks to evict old connections.
	fmt.Println()
	fmt.Println("=== Phase 2: waiting for health checks ===")
	time.Sleep(350 * time.Millisecond)

	// Phase 3: use connections again (should get replacement connections).
	fmt.Println()
	fmt.Println("=== Phase 3: using replacement connections ===")
	for i := 4; i <= 6; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			req := NewAcquireRequest()
			acquire <- req
			conn := <-req.Reply
			if conn == nil {
				return
			}
			fmt.Printf("  worker %d: using conn %d (created: %v ago)\n",
				id, conn.ID, time.Since(conn.CreatedAt).Round(time.Millisecond))
			time.Sleep(30 * time.Millisecond)
			release <- conn
		}(i)
	}
	wg.Wait()

	close(done)
}
```

During Phase 2, the ticker fires and the health check evicts connections older than 250ms. Replacement connections are created. In Phase 3, workers get the fresh replacement connections.

### Verification
```bash
go run -race main.go
```
Expected output (approximate):
```
[pool] initialized: 4 connections
=== Phase 1: normal usage ===
  worker 1: using conn 4
  worker 2: using conn 3
  worker 3: using conn 2
[health] check complete: 4 idle, 0 evicted

=== Phase 2: waiting for health checks ===
[health] evicted conn 1 (age: 300ms)
[health] evicted conn 2 (age: 300ms)
[health] evicted conn 3 (age: 300ms)
[health] evicted conn 4 (age: 300ms)
[health] created replacement conn 5
[health] created replacement conn 6
[health] created replacement conn 7
[health] created replacement conn 8
[health] check complete: 4 idle, 4 evicted

=== Phase 3: using replacement connections ===
  worker 4: using conn 8 (created: 10ms ago)
  worker 5: using conn 7 (created: 10ms ago)
  worker 6: using conn 6 (created: 10ms ago)
[pool] shutdown: created=8, evicted=4
```

## Step 4 -- Connection Replacement on Failure

Handle the case where a connection fails during use. The worker reports a bad connection on a separate channel, and the pool replaces it without the worker needing to retry manually.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

const (
	fullPoolSize          = 3
	fullHealthPeriod      = 150 * time.Millisecond
	fullConnMaxAge        = 500 * time.Millisecond
	failureProbability    = 30
	fullWorkerCount       = 10
)

var fullConnID atomic.Int64

type Conn struct {
	ID        int64
	CreatedAt time.Time
}

func NewConn() *Conn {
	return &Conn{ID: fullConnID.Add(1), CreatedAt: time.Now()}
}

func (c *Conn) IsHealthy() bool {
	return time.Since(c.CreatedAt) < fullConnMaxAge
}

type AcquireRequest struct {
	Reply chan *Conn
}

func NewAcquireRequest() AcquireRequest {
	return AcquireRequest{Reply: make(chan *Conn, 1)}
}

// BadConnReport signals that a connection failed during use.
type BadConnReport struct {
	ConnID int64
	Reply  chan *Conn
}

// fullPoolManager handles acquire, release, bad connections, and health checks.
func fullPoolManager(
	acquire <-chan AcquireRequest,
	release <-chan *Conn,
	badConn <-chan BadConnReport,
	done <-chan struct{},
	healthTick <-chan time.Time,
) {
	idle := make([]*Conn, 0, fullPoolSize)
	for i := 0; i < fullPoolSize; i++ {
		idle = append(idle, NewConn())
	}

	waiters := make([]AcquireRequest, 0)
	stats := struct {
		created, evicted, badReports int
	}{created: fullPoolSize}

	serveOrQueue := func(req AcquireRequest) {
		if len(idle) > 0 {
			conn := idle[len(idle)-1]
			idle = idle[:len(idle)-1]
			req.Reply <- conn
		} else {
			waiters = append(waiters, req)
		}
	}

	returnToPool := func(conn *Conn) {
		if len(waiters) > 0 {
			next := waiters[0]
			waiters = waiters[1:]
			next.Reply <- conn
		} else {
			idle = append(idle, conn)
		}
	}

	for {
		select {
		case <-done:
			for _, w := range waiters {
				w.Reply <- nil
			}
			fmt.Printf("[pool] shutdown: created=%d, evicted=%d, bad_reports=%d\n",
				stats.created, stats.evicted, stats.badReports)
			return

		case req := <-acquire:
			serveOrQueue(req)

		case conn := <-release:
			returnToPool(conn)

		case report := <-badConn:
			stats.badReports++
			replacement := NewConn()
			stats.created++
			fmt.Printf("[pool] bad conn %d replaced with conn %d\n",
				report.ConnID, replacement.ID)
			report.Reply <- replacement

		case <-healthTick:
			healthy := make([]*Conn, 0, len(idle))
			for _, conn := range idle {
				if conn.IsHealthy() {
					healthy = append(healthy, conn)
				} else {
					stats.evicted++
					replacement := NewConn()
					stats.created++
					healthy = append(healthy, replacement)
				}
			}
			idle = healthy
		}
	}
}

func main() {
	acquire := make(chan AcquireRequest, 20)
	release := make(chan *Conn, 20)
	badConn := make(chan BadConnReport, 10)
	done := make(chan struct{})
	ticker := time.NewTicker(fullHealthPeriod)
	defer ticker.Stop()

	go fullPoolManager(acquire, release, badConn, done, ticker.C)

	var wg sync.WaitGroup
	rng := rand.New(rand.NewPCG(42, 0))

	successCount := 0
	failCount := 0
	var mu sync.Mutex

	for i := 1; i <= fullWorkerCount; i++ {
		wg.Add(1)
		willFail := rng.IntN(100) < failureProbability
		go func(workerID int, simulateFail bool) {
			defer wg.Done()
			req := NewAcquireRequest()
			acquire <- req
			conn := <-req.Reply
			if conn == nil {
				return
			}

			time.Sleep(30 * time.Millisecond)

			if simulateFail {
				fmt.Printf("  worker %d: conn %d failed during query\n", workerID, conn.ID)
				report := BadConnReport{ConnID: conn.ID, Reply: make(chan *Conn, 1)}
				badConn <- report
				replacement := <-report.Reply
				fmt.Printf("  worker %d: got replacement conn %d\n", workerID, replacement.ID)
				time.Sleep(20 * time.Millisecond)
				release <- replacement
				mu.Lock()
				failCount++
				mu.Unlock()
			} else {
				release <- conn
				mu.Lock()
				successCount++
				mu.Unlock()
			}
			fmt.Printf("  worker %d: done\n", workerID)
		}(i, willFail)
	}

	wg.Wait()
	close(done)

	fmt.Printf("\n=== Results ===\n")
	fmt.Printf("  workers: %d, success: %d, recovered: %d\n",
		fullWorkerCount, successCount, failCount)
}
```

When a connection fails, the worker does not release it (it is dead). Instead, it sends a `BadConnReport`. The pool creates a replacement and sends it back on the report's reply channel. The worker continues with the replacement connection.

### Verification
```bash
go run -race main.go
```
Expected output (approximate, depends on RNG seed):
```
  worker 1: conn 3 failed during query
[pool] bad conn 3 replaced with conn 4
  worker 1: got replacement conn 4
  worker 2: done
  worker 3: done
  ...
  worker 10: done

=== Results ===
  workers: 10, success: 7, recovered: 3
[pool] shutdown: created=6, evicted=0, bad_reports=3
```

## Common Mistakes

### Returning Dead Connections to the Pool
**What happens:** If a worker detects a connection failure but still releases it to the pool via the release channel, the next worker that acquires it will also fail. The dead connection bounces between workers.

**Fix:** Use a separate `badConn` channel for failed connections. The pool manager replaces them instead of putting them back in the idle list.

### Forgetting to Unblock Waiters on Shutdown
**What happens:** When the pool manager exits without sending to queued waiters' reply channels, those goroutines block on `<-req.Reply` forever, leaking goroutines.

**Fix:** On shutdown, iterate over all waiters and send nil to their reply channels:
```go
case <-done:
    for _, w := range waiters {
        w.Reply <- nil
    }
    return
```

### Health Check Running While Connections Are In Use
**What happens:** The health check only examines the idle list. Connections currently in use by workers are not in the idle list, so they are not checked. This is correct behavior -- but a common misunderstanding is to try to check all connections, which would require tracking checked-out connections and introduces complexity.

**Fix:** Health checks only validate idle connections. If a connection fails during use, the worker reports it via the `badConn` channel. This separation keeps the manager simple.

## Review

The design's power comes from a single decision: one manager goroutine owns the
entire idle list, and every operation is a message rather than a shared-memory
access. Acquire sends a request carrying its own reply channel and blocks on
that reply; release sends the connection back; a `time.Ticker` feeds a
health-check case; a separate bad-connection channel carries in-use failures.
Because all five events -- acquire, release, health tick, bad-conn, shutdown --
arrive at the same `select` loop, the manager handles them one at a time and no
lock is ever needed. When the pool is exhausted the manager parks the request
in a waiter queue and serves it FIFO the moment a connection returns, so
callers block instead of failing. Dead connections found by the health check
are evicted and replaced to hold the pool at its target size, and the bad-conn
channel keeps a connection that failed mid-query from being handed to the next
caller.

The two extensions the exercise leaves you are natural next messages on the
same loop. An acquire timeout means the caller selects over its reply channel
and a `time.After`, and on timeout the manager must drop that waiter from the
queue so a later release does not hand a connection to a caller who already
gave up. Pool metrics -- average wait time, utilization as in-use versus idle,
and lifetime eviction count -- are just more state the manager owns and updates
as those same events pass through, reported without a lock because only the
manager ever touches them.

## Resources
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the talk that popularized the single-owner-goroutine-plus-channels design this pool uses.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- why passing ownership over a channel removes the need for a mutex.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- reference for the channel operations and select behavior the manager loop relies on.
- [database/sql Pool Implementation](https://pkg.go.dev/database/sql) -- the standard library's production connection pool, the real-world version of this exercise.

---

Back to [Concurrency](../../concurrency.md) | Next: [32-channel-request-coalescing](../32-channel-request-coalescing/32-channel-request-coalescing.md)
