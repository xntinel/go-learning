# Exercise 18: Connection Pool

Every production service that talks to a database, cache, or external API needs
connection pooling. Creating a fresh connection per request is expensive -- TCP
handshake, TLS negotiation, authentication -- so you keep a fixed pool of
reusable connections and hand them out. The hard part is that hundreds of
request-handling goroutines must share a handful of connections safely, block
politely when none are free, and never lose or duplicate one. Get the handoff
wrong and you get a connection leak that wedges the whole service.

## What you'll build

```text
18-connection-pool/
  main.go        a fixed-size Pool backed by a buffered channel: acquire,
                 release, contention timing, and health-based replacement
```

- Build: a fixed-size connection pool backed by a buffered channel, exercised under concurrent load.
- Implement: `Pool` with `Acquire`, `Release`, and `Available`; a concurrent request simulation; a timing analysis that reveals the wave pattern; and health-checking that retires and replaces degraded connections.
- Verify: `go run main.go` for each step.

### Why a buffered channel is the pool

A buffered channel is a natural fit for this pattern in Go. The channel's
capacity represents the pool size. Acquiring a connection is a channel receive
(blocks when empty), and releasing is a channel send (blocks when full, which
should never happen in a well-designed pool). The Go runtime handles all the
queuing and wake-up logic -- you get a thread-safe pool with zero explicit
locking.

This pattern appears in production libraries like `database/sql`'s connection
pool, Redis client pools, and HTTP connection pools. Understanding the mechanics
here prepares you to reason about pool exhaustion, connection leaks, and health
checking in real systems.


## Step 1 -- Basic Pool with Single Goroutine

Start with the simplest possible pool: create it, acquire one connection, use it, release it. This establishes the core data structures and channel-based acquire/release pattern.

```go
package main

import (
	"fmt"
	"time"
)

type Connection struct {
	ID        int
	CreatedAt time.Time
	UseCount  int
}

func (c *Connection) Execute(query string) string {
	c.UseCount++
	time.Sleep(50 * time.Millisecond)
	return fmt.Sprintf("[conn-%d] result for: %s (use #%d)", c.ID, query, c.UseCount)
}

type Pool struct {
	connections chan *Connection
	size        int
}

func NewPool(size int) *Pool {
	p := &Pool{
		connections: make(chan *Connection, size),
		size:        size,
	}
	for i := 1; i <= size; i++ {
		p.connections <- &Connection{
			ID:        i,
			CreatedAt: time.Now(),
		}
	}
	return p
}

func (p *Pool) Acquire() *Connection {
	return <-p.connections
}

func (p *Pool) Release(c *Connection) {
	p.connections <- c
}

func (p *Pool) Available() int {
	return len(p.connections)
}

func main() {
	pool := NewPool(5)
	fmt.Printf("Pool created with %d connections\n", pool.size)
	fmt.Printf("Available: %d\n\n", pool.Available())

	conn := pool.Acquire()
	fmt.Printf("Acquired connection %d (available: %d)\n", conn.ID, pool.Available())

	result := conn.Execute("SELECT * FROM users LIMIT 10")
	fmt.Printf("Result: %s\n", result)

	pool.Release(conn)
	fmt.Printf("Released connection %d (available: %d)\n", conn.ID, pool.Available())
}
```

**What's happening here:** `NewPool` creates a buffered channel of size 5 and fills it with 5 `Connection` pointers. `Acquire` reads from the channel (blocks if empty), and `Release` writes back to the channel. With a single goroutine, there's no contention -- this just proves the acquire/release cycle works.

**Key insight:** The buffered channel acts as both a storage container and a synchronization primitive. When empty, `Acquire` blocks the calling goroutine until another goroutine calls `Release`. This blocking behavior is exactly what you want -- it naturally throttles concurrent access to the limited resource.

### Verification
```bash
go run main.go
```
Expected output:
```
Pool created with 5 connections
Available: 5

Acquired connection 1 (available: 4)
Result: [conn-1] result for: SELECT * FROM users LIMIT 10 (use #1)
Released connection 1 (available: 5)
```


## Step 2 -- Concurrent Request Goroutines Competing for Connections

Launch 20 request goroutines that all compete for 5 connections. Observe how goroutines queue up when the pool is exhausted, and how connections are reused across requests.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type Connection struct {
	ID        int
	CreatedAt time.Time
	UseCount  int
}

func (c *Connection) Execute(query string) string {
	c.UseCount++
	time.Sleep(50 * time.Millisecond)
	return fmt.Sprintf("[conn-%d] %s (use #%d)", c.ID, query, c.UseCount)
}

type Pool struct {
	connections chan *Connection
	size        int
}

func NewPool(size int) *Pool {
	p := &Pool{
		connections: make(chan *Connection, size),
		size:        size,
	}
	for i := 1; i <= size; i++ {
		p.connections <- &Connection{
			ID:        i,
			CreatedAt: time.Now(),
		}
	}
	return p
}

func (p *Pool) Acquire() *Connection {
	return <-p.connections
}

func (p *Pool) Release(c *Connection) {
	p.connections <- c
}

func (p *Pool) Available() int {
	return len(p.connections)
}

const (
	poolSize     = 5
	requestCount = 20
)

func simulateRequest(id int, pool *Pool, wg *sync.WaitGroup) {
	defer wg.Done()

	waitStart := time.Now()
	conn := pool.Acquire()
	waitTime := time.Since(waitStart)

	result := conn.Execute(fmt.Sprintf("query-%d", id))

	pool.Release(conn)

	fmt.Printf("  req-%02d | waited %6v | %s\n", id, waitTime.Round(time.Millisecond), result)
}

func main() {
	pool := NewPool(poolSize)
	fmt.Printf("Pool: %d connections | Requests: %d\n\n", poolSize, requestCount)

	start := time.Now()
	var wg sync.WaitGroup

	for i := 1; i <= requestCount; i++ {
		wg.Add(1)
		go simulateRequest(i, pool, &wg)
	}

	wg.Wait()
	elapsed := time.Since(start)

	fmt.Printf("\n--- Summary ---\n")
	fmt.Printf("  All %d requests completed in %v\n", requestCount, elapsed.Round(time.Millisecond))
	fmt.Printf("  Sequential would have taken: ~%v\n",
		time.Duration(requestCount)*50*time.Millisecond)
	fmt.Printf("  Theoretical minimum (20 reqs / 5 conns): ~%v\n",
		time.Duration(requestCount/poolSize)*50*time.Millisecond)
	fmt.Printf("  Pool available: %d\n", pool.Available())
}
```

**What's happening here:** All 20 goroutines launch nearly simultaneously. The first 5 acquire connections immediately. The remaining 15 block on `pool.Acquire()` until a connection is released. As each connection is released after its 50ms query, the next waiting goroutine wakes up and acquires it. The pool acts as a natural throttle, allowing at most 5 concurrent database operations.

**Key insight:** The `waitTime` measurement shows the queuing effect. Early goroutines wait ~0ms (connections available immediately). Later goroutines wait 50-200ms as they queue behind others. This is the same queuing behavior you'd see in a real database connection pool under load.

### Verification
```bash
go run main.go
```
Expected output (order varies):
```
Pool: 5 connections | Requests: 20

  req-03 | waited    0ms | [conn-3] query-3 (use #1)
  req-01 | waited    0ms | [conn-1] query-1 (use #1)
  req-05 | waited    0ms | [conn-5] query-5 (use #1)
  req-02 | waited    0ms | [conn-2] query-2 (use #1)
  req-04 | waited    0ms | [conn-4] query-4 (use #1)
  req-08 | waited   50ms | [conn-3] query-8 (use #2)
  req-06 | waited   50ms | [conn-1] query-6 (use #2)
  req-10 | waited   50ms | [conn-5] query-10 (use #2)
  req-07 | waited   50ms | [conn-2] query-7 (use #2)
  req-09 | waited   51ms | [conn-4] query-9 (use #2)
  ...
  req-20 | waited  151ms | [conn-2] query-20 (use #4)

--- Summary ---
  All 20 requests completed in 200ms
  Sequential would have taken: ~1s
  Theoretical minimum (20 reqs / 5 conns): ~200ms
  Pool available: 5
```


## Step 3 -- Timing Analysis of Pool Contention

Add detailed timing to visualize exactly when each goroutine acquires and releases its connection. This reveals the wave pattern of pool utilization.

```go
package main

import (
	"cmp"
	"fmt"
	"slices"
	"sync"
	"time"
)

type Connection struct {
	ID        int
	CreatedAt time.Time
	UseCount  int
}

func (c *Connection) Execute(query string) string {
	c.UseCount++
	time.Sleep(50 * time.Millisecond)
	return fmt.Sprintf("conn-%d use #%d", c.ID, c.UseCount)
}

type Pool struct {
	connections chan *Connection
	size        int
}

func NewPool(size int) *Pool {
	p := &Pool{
		connections: make(chan *Connection, size),
		size:        size,
	}
	for i := 1; i <= size; i++ {
		p.connections <- &Connection{
			ID:        i,
			CreatedAt: time.Now(),
		}
	}
	return p
}

func (p *Pool) Acquire() *Connection { return <-p.connections }
func (p *Pool) Release(c *Connection) { p.connections <- c }

const (
	poolSize     = 5
	requestCount = 20
)

type RequestLog struct {
	RequestID  int
	ConnID     int
	WaitTime   time.Duration
	AcquireAt  time.Duration
	ReleaseAt  time.Duration
}

func main() {
	pool := NewPool(poolSize)
	origin := time.Now()

	logs := make(chan RequestLog, requestCount)
	var wg sync.WaitGroup

	for i := 1; i <= requestCount; i++ {
		wg.Add(1)
		go func(reqID int) {
			defer wg.Done()

			waitStart := time.Now()
			conn := pool.Acquire()
			acquireAt := time.Since(origin)
			waitTime := time.Since(waitStart)

			conn.Execute(fmt.Sprintf("query-%d", reqID))

			pool.Release(conn)
			releaseAt := time.Since(origin)

			logs <- RequestLog{
				RequestID: reqID,
				ConnID:    conn.ID,
				WaitTime:  waitTime,
				AcquireAt: acquireAt,
				ReleaseAt: releaseAt,
			}
		}(i)
	}

	wg.Wait()
	close(logs)

	var allLogs []RequestLog
	for l := range logs {
		allLogs = append(allLogs, l)
	}

	slices.SortFunc(allLogs, func(a, b RequestLog) int {
		return cmp.Compare(a.AcquireAt, b.AcquireAt)
	})

	fmt.Printf("=== Pool Contention Timeline (%d pool / %d requests) ===\n\n", poolSize, requestCount)
	fmt.Printf("  %-6s %-7s %10s %10s %10s\n", "Req", "Conn", "Waited", "Acquired", "Released")
	fmt.Printf("  %-6s %-7s %10s %10s %10s\n", "---", "----", "------", "--------", "--------")

	for _, l := range allLogs {
		fmt.Printf("  req-%02d conn-%d %10v %10v %10v\n",
			l.RequestID, l.ConnID,
			l.WaitTime.Round(time.Millisecond),
			l.AcquireAt.Round(time.Millisecond),
			l.ReleaseAt.Round(time.Millisecond))
	}

	connUsage := make(map[int]int)
	var maxWait time.Duration
	for _, l := range allLogs {
		connUsage[l.ConnID]++
		if l.WaitTime > maxWait {
			maxWait = l.WaitTime
		}
	}

	fmt.Printf("\n--- Connection Utilization ---\n")
	for id := 1; id <= poolSize; id++ {
		bar := ""
		for j := 0; j < connUsage[id]; j++ {
			bar += "##"
		}
		fmt.Printf("  conn-%d: %d uses %s\n", id, connUsage[id], bar)
	}

	fmt.Printf("\n--- Contention Stats ---\n")
	fmt.Printf("  Max wait time: %v\n", maxWait.Round(time.Millisecond))
	fmt.Printf("  Total elapsed:  %v\n", allLogs[len(allLogs)-1].ReleaseAt.Round(time.Millisecond))
	fmt.Printf("  Waves: %d (20 requests / 5 connections)\n", requestCount/poolSize)
}
```

**What's happening here:** Each goroutine records when it acquired and released its connection relative to the program start. Sorting by acquire time reveals the wave pattern: requests 1-5 acquire at ~0ms, requests 6-10 at ~50ms (when the first wave releases), requests 11-15 at ~100ms, and requests 16-20 at ~150ms. The connection utilization shows that each connection serves exactly 4 requests (20 / 5).

**Key insight:** The timeline makes the contention model concrete. Pool-based concurrency is not about running everything at once -- it is about controlling how many things run simultaneously. This is the same throttling model used by `database/sql.SetMaxOpenConns()`, HTTP client pools, and worker pool patterns.

### Verification
```bash
go run main.go
```
Expected output (IDs and exact times vary):
```
=== Pool Contention Timeline (5 pool / 20 requests) ===

  Req    Conn        Waited   Acquired   Released
  ---    ----        ------   --------   --------
  req-02 conn-2        0ms        0ms       50ms
  req-05 conn-5        0ms        0ms       50ms
  req-03 conn-3        0ms        0ms       50ms
  req-01 conn-1        0ms        0ms       50ms
  req-04 conn-4        0ms        0ms       50ms
  req-07 conn-2       50ms       50ms      100ms
  req-10 conn-5       50ms       50ms      100ms
  ...
  req-18 conn-3      150ms      150ms      200ms
  req-16 conn-1      150ms      150ms      200ms
  req-20 conn-4      150ms      150ms      200ms

--- Connection Utilization ---
  conn-1: 4 uses ########
  conn-2: 4 uses ########
  conn-3: 4 uses ########
  conn-4: 4 uses ########
  conn-5: 4 uses ########

--- Contention Stats ---
  Max wait time: 150ms
  Total elapsed:  200ms
  Waves: 4 (20 requests / 5 connections)
```


## Step 4 -- Health Checking and Connection Replacement

Add a maximum use count per connection. When a connection has been used too many times, replace it with a fresh one before returning it to the pool. This simulates production connection pools that retire stale connections.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	poolSize       = 5
	requestCount   = 20
	maxUsePerConn  = 3
)

var nextConnID int64

type Connection struct {
	ID        int
	CreatedAt time.Time
	UseCount  int
}

func newConnection() *Connection {
	id := int(atomic.AddInt64(&nextConnID, 1))
	return &Connection{
		ID:        id,
		CreatedAt: time.Now(),
	}
}

func (c *Connection) Execute(query string) string {
	c.UseCount++
	time.Sleep(50 * time.Millisecond)
	return fmt.Sprintf("conn-%d use #%d", c.ID, c.UseCount)
}

func (c *Connection) IsHealthy() bool {
	return c.UseCount < maxUsePerConn
}

type Pool struct {
	connections chan *Connection
	size        int
	replaced    int64
}

func NewPool(size int) *Pool {
	p := &Pool{
		connections: make(chan *Connection, size),
		size:        size,
	}
	for i := 0; i < size; i++ {
		p.connections <- newConnection()
	}
	return p
}

func (p *Pool) Acquire() *Connection {
	return <-p.connections
}

func (p *Pool) Release(c *Connection) {
	if !c.IsHealthy() {
		atomic.AddInt64(&p.replaced, 1)
		fresh := newConnection()
		fmt.Printf("    [pool] conn-%d retired after %d uses, replaced with conn-%d\n",
			c.ID, c.UseCount, fresh.ID)
		p.connections <- fresh
		return
	}
	p.connections <- c
}

func (p *Pool) Replaced() int64 {
	return atomic.LoadInt64(&p.replaced)
}

type RequestResult struct {
	RequestID int
	ConnID    int
	ConnUse   int
	WaitTime  time.Duration
}

func main() {
	pool := NewPool(poolSize)
	fmt.Printf("Pool: %d connections | Max uses per conn: %d | Requests: %d\n\n",
		poolSize, maxUsePerConn, requestCount)

	results := make(chan RequestResult, requestCount)
	var wg sync.WaitGroup
	start := time.Now()

	for i := 1; i <= requestCount; i++ {
		wg.Add(1)
		go func(reqID int) {
			defer wg.Done()

			waitStart := time.Now()
			conn := pool.Acquire()
			waitTime := time.Since(waitStart)

			conn.Execute(fmt.Sprintf("query-%d", reqID))

			connID := conn.ID
			connUse := conn.UseCount

			pool.Release(conn)

			results <- RequestResult{
				RequestID: reqID,
				ConnID:    connID,
				ConnUse:   connUse,
				WaitTime:  waitTime,
			}
		}(i)
	}

	wg.Wait()
	close(results)
	elapsed := time.Since(start)

	var allResults []RequestResult
	for r := range results {
		allResults = append(allResults, r)
	}

	fmt.Printf("\n--- Results ---\n")
	fmt.Printf("  %-6s %-8s %-8s %s\n", "Req", "Conn", "ConnUse", "Wait")
	for _, r := range allResults {
		fmt.Printf("  req-%02d conn-%-3d use %-3d %v\n",
			r.RequestID, r.ConnID, r.ConnUse, r.WaitTime.Round(time.Millisecond))
	}

	fmt.Printf("\n--- Health Check Summary ---\n")
	fmt.Printf("  Total requests:       %d\n", requestCount)
	fmt.Printf("  Connections replaced:  %d\n", pool.Replaced())
	fmt.Printf("  Total elapsed:         %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  Unique connection IDs: original 1-%d, new ones created on replacement\n", poolSize)
}
```

**What's happening here:** Each connection tracks its use count. When `Release` is called, the pool checks `IsHealthy()`. If the connection has reached `maxUsePerConn` (3), it is discarded and replaced with a fresh one. The `atomic.AddInt64` for `nextConnID` ensures unique IDs even when replacements happen concurrently. You'll see connection IDs above 5 appear as old connections are retired.

**Key insight:** The health check happens at release time, not acquire time. This is a deliberate design choice: the goroutine that just used the connection pays the replacement cost, rather than the next goroutine that needs a connection. In production, you might also check connection health at acquire time (e.g., ping the database), but release-time checks for usage limits are cheaper and predictable.

### Verification
```bash
go run main.go
```
Expected output (order and IDs vary):
```
Pool: 5 connections | Max uses per conn: 3 | Requests: 20

    [pool] conn-1 retired after 3 uses, replaced with conn-6
    [pool] conn-3 retired after 3 uses, replaced with conn-7
    [pool] conn-2 retired after 3 uses, replaced with conn-8
    [pool] conn-5 retired after 3 uses, replaced with conn-9
    [pool] conn-4 retired after 3 uses, replaced with conn-10
    [pool] conn-6 retired after 3 uses, replaced with conn-11
    [pool] conn-7 retired after 3 uses, replaced with conn-12

--- Results ---
  Req    Conn     ConnUse  Wait
  req-03 conn-3   use 1   0ms
  req-01 conn-1   use 1   0ms
  req-05 conn-5   use 1   0ms
  ...

--- Health Check Summary ---
  Total requests:       20
  Connections replaced:  7
  Total elapsed:         200ms
  Unique connection IDs: original 1-5, new ones created on replacement
```


## Common Mistakes

### Forgetting to Release Connections (Connection Leak)

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type Connection struct{ ID int }

func main() {
	pool := make(chan *Connection, 3)
	for i := 1; i <= 3; i++ {
		pool <- &Connection{ID: i}
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(reqID int) {
			defer wg.Done()
			conn := <-pool
			fmt.Printf("req-%d acquired conn-%d\n", reqID, conn.ID)
			time.Sleep(50 * time.Millisecond)
			// BUG: never releases connection back to pool
		}(i)
	}
	wg.Wait() // deadlock: requests 3 and 4 wait forever for a connection
}
```
**What happens:** The first 3 goroutines acquire connections but never release them. Goroutines 3 and 4 block forever on `<-pool`. This is a connection leak -- the most common pool-related bug in production. The `wg.Wait()` deadlocks because goroutines 3 and 4 never finish.

**Correct -- always release with defer:**
```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type Connection struct{ ID int }

func main() {
	pool := make(chan *Connection, 3)
	for i := 1; i <= 3; i++ {
		pool <- &Connection{ID: i}
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(reqID int) {
			defer wg.Done()
			conn := <-pool
			defer func() { pool <- conn }() // always release

			fmt.Printf("req-%d acquired conn-%d\n", reqID, conn.ID)
			time.Sleep(50 * time.Millisecond)
		}(i)
	}
	wg.Wait()
	fmt.Printf("Pool available: %d\n", len(pool))
}
```

### Releasing a Connection Twice

**Wrong -- complete program:**
```go
package main

import "fmt"

type Connection struct{ ID int }

func main() {
	pool := make(chan *Connection, 2)
	pool <- &Connection{ID: 1}
	pool <- &Connection{ID: 2}

	conn := <-pool
	fmt.Printf("Acquired conn-%d (available: %d)\n", conn.ID, len(pool))
	pool <- conn // release once
	pool <- conn // release again -- pool now has 3 items in a size-2 channel!
	// This blocks forever because the channel is full
}
```
**What happens:** Releasing twice either blocks (if channel is full) or corrupts the pool by having duplicate references. Two goroutines could acquire the "same" connection simultaneously, causing data corruption.

**Correct -- ensure single release per acquire:**
```go
package main

import "fmt"

type Connection struct{ ID int }

type Pool struct {
	conns chan *Connection
}

func (p *Pool) Acquire() *Connection { return <-p.conns }
func (p *Pool) Release(c *Connection) { p.conns <- c }

func main() {
	p := &Pool{conns: make(chan *Connection, 2)}
	p.conns <- &Connection{ID: 1}
	p.conns <- &Connection{ID: 2}

	conn := p.Acquire()
	fmt.Printf("Acquired conn-%d (available: %d)\n", conn.ID, len(p.conns))
	// Use the connection...
	p.Release(conn) // exactly one release per acquire
	fmt.Printf("Released conn-%d (available: %d)\n", conn.ID, len(p.conns))
}
```


## Review

A buffered channel is the whole pool: its capacity is the pool size, an
`Acquire` is a receive that blocks when the pool is empty, and a `Release` is a
send that returns the connection for the next waiter. Because the runtime owns
the queuing and wake-up, you get a thread-safe pool with no explicit locking --
and the wave pattern you measured (N connections busy, the rest of the goroutines
parked, then the next N) is simply that blocking made visible. The two rules that
keep it correct are exactly one release per acquire (best guaranteed with
`defer`, or a leak wedges the service) and single ownership of each connection
(a double release corrupts the pool). Health checking at release time lets a
worn-out connection be retired and replaced transparently, with the goroutine
that just used it paying the cost rather than the next one waiting.

To confirm the model sticks, build a rate-limited API client: a pool of 3
clients each with a unique ID, 15 goroutines that each acquire a client, make a
100ms call, and release it, tracking per-client use count and per-request wait
time. Print a timeline that shows the five waves of three concurrent calls, and
add a maximum-uses-per-client limit of 4 that replaces a client once it is
exhausted. Use `defer` so the client is returned even if the call panics, and
timestamp with `time.Since(origin)` at acquire and release to draw the waves.

## Resources

- [Go Spec: Channel types](https://go.dev/ref/spec#Channel_types) -- the send/receive and buffering semantics that make a channel behave as a fixed-size pool.
- [database/sql: SetMaxOpenConns](https://pkg.go.dev/database/sql#DB.SetMaxOpenConns) -- the standard-library pool this exercise mirrors, and the knob that caps its size.
- [Effective Go: Channels as semaphores](https://go.dev/doc/effective_go#chan_of_chan) -- using a buffered channel to bound concurrent access to a limited resource.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the philosophy behind passing ownership through channels rather than locking shared state.

---

Back to [Concurrency](../../concurrency.md) | Next: [19-parallel-validation](../19-parallel-validation/19-parallel-validation.md)
