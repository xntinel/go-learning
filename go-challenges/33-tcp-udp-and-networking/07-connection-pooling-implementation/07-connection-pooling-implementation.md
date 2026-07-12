# 7. Connection Pooling Implementation

Opening a TCP connection involves a three-way SYN handshake and, for TLS, an additional round-trip plus certificate verification. When an application sends many short-lived requests to the same server — a database, a cache, a downstream service — paying that cost on every request adds measurable latency. A connection pool keeps a bounded set of already-open connections and hands them out on demand. The hard part is not storing connections in a slice; it is enforcing two independent limits (total open, total idle) concurrently, blocking callers fairly when both limits are hit, and discarding stale connections without holding locks during I/O.

## Concepts

### What the Pool Manages

A pool tracks two orthogonal counts:

- **Open** — every connection the pool owns, whether in use by a caller or sitting idle. Bounded by `MaxOpen` to protect the server from too many simultaneous connections.
- **Idle** — connections that have been returned and are waiting to be reused. Bounded by `MaxIdle` (always <= `MaxOpen`) to cap client-side memory.

A connection is in one of three states: idle (in the pool's list), in-use (held by a caller), or closed. The invariant `open = in-use + idle` must hold at all times. `Get` moves a connection from idle to in-use (or dials a new one); `Put` moves it from in-use back to idle.

### LIFO Idle Stack Over FIFO Queue

Idle connections are kept on a stack (last-in, first-out). The connection returned most recently is reused first. This keeps the working set small under light load: one connection handles sequential requests rather than all connections staying warm indefinitely. A FIFO queue would distribute reuse evenly across all idle connections, keeping every one of them alive and consuming file descriptors on the server. Go's `net/http.Transport` and `database/sql` both use LIFO for idle connections.

### Blocking on Exhaustion With a Channel Semaphore

When `open == MaxOpen` and the idle list is empty, the caller must wait for another caller to call `Put`. A buffered `chan struct{}` of capacity one acts as a lightweight semaphore. `Put` sends to it (non-blocking); waiting `Get` callers `select` on it alongside `ctx.Done()` and the pool's close signal. This gives the waiter precise cancellation semantics without a spin loop or a condition variable.

### Health Checks and Idle Timeouts

An idle connection can go stale: the server may close it while it sits unused, and the OS will not surface the RST until the next write attempt. Two defenses:

1. **Idle timeout** — discard any connection whose idle time exceeds `IdleTimeout`. The pool checks the timestamp lazily, on the next `Get` that would return it, rather than on a background timer. Lazy eviction is simpler and correct because callers would discard a stale connection anyway.
2. **Health check** — an optional `func(net.Conn) error`. A failed check causes the connection to be closed and the pool to try the next idle slot or dial fresh.

Both checks run while holding the pool lock, so they must be fast. A health check that does blocking I/O should use `SetReadDeadline` with a short deadline before attempting the read.

### Closing the Pool

`Close` sets a closed flag, drains the idle list, and calls `close(closeCh)`. Closing a channel is a broadcast: every goroutine blocked on `<-closeCh` wakes simultaneously. This is the key difference from sending to a channel — a send unblocks exactly one waiter; a close unblocks all of them.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/33-tcp-udp-and-networking/07-connection-pooling-implementation/07-connection-pooling-implementation/cmd/demo
cd go-solutions/33-tcp-udp-and-networking/07-connection-pooling-implementation/07-connection-pooling-implementation
```

This is a library, not a program. Verification is via `go test`.

### Exercise 1: The Pool Type and Constructor

Create `pool.go`:

```go
package pool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// ErrPoolClosed is returned by Get when the pool has been closed.
var ErrPoolClosed = errors.New("pool: closed")

// PoolOptions configures a Pool.
type PoolOptions struct {
	MaxOpen     int                  // max total open connections; default 10
	MaxIdle     int                  // max idle connections; default 5
	IdleTimeout time.Duration        // idle eviction threshold; default 60s
	DialTimeout time.Duration        // per-dial timeout; default 5s
	HealthCheck func(net.Conn) error // optional; nil means skip
}

// Stats is a point-in-time snapshot of pool activity.
type Stats struct {
	Open    int // total connections owned by the pool (idle + in-use)
	Idle    int // connections waiting in the idle pool
	Waiting int // goroutines currently blocked in Get
}

type idleConn struct {
	conn      net.Conn
	idleSince time.Time
}

// Pool manages a bounded set of reusable TCP connections to a single address.
type Pool struct {
	mu      sync.Mutex
	addr    string
	opts    PoolOptions
	idle    []idleConn
	open    int
	waiting int
	waitCh  chan struct{}
	closeCh chan struct{}
	closed  bool
}

// New returns a Pool that dials addr on demand.
// Zero-value fields in opts receive safe defaults.
func New(addr string, opts PoolOptions) *Pool {
	if opts.MaxOpen <= 0 {
		opts.MaxOpen = 10
	}
	if opts.MaxIdle <= 0 {
		opts.MaxIdle = 5
	}
	if opts.MaxIdle > opts.MaxOpen {
		opts.MaxIdle = opts.MaxOpen
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 60 * time.Second
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 5 * time.Second
	}
	return &Pool{
		addr:    addr,
		opts:    opts,
		waitCh:  make(chan struct{}, 1),
		closeCh: make(chan struct{}),
	}
}

// Get acquires a connection. It reuses an idle connection when one is
// available, dials a new one when MaxOpen is not reached, or blocks until
// a connection is returned or ctx is cancelled.
func (p *Pool) Get(ctx context.Context) (net.Conn, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrPoolClosed
		}

		// Drain idle connections, skipping any that are expired or unhealthy.
		for len(p.idle) > 0 {
			ic := p.idle[len(p.idle)-1]
			p.idle = p.idle[:len(p.idle)-1]

			if time.Since(ic.idleSince) > p.opts.IdleTimeout {
				ic.conn.Close()
				p.open--
				continue
			}
			if p.opts.HealthCheck != nil {
				if err := p.opts.HealthCheck(ic.conn); err != nil {
					ic.conn.Close()
					p.open--
					continue
				}
			}
			p.mu.Unlock()
			return ic.conn, nil
		}

		// Idle list is empty. Dial if below the open cap.
		if p.open < p.opts.MaxOpen {
			p.open++
			p.mu.Unlock()
			conn, err := net.DialTimeout("tcp", p.addr, p.opts.DialTimeout)
			if err != nil {
				p.mu.Lock()
				p.open--
				p.mu.Unlock()
				// A slot just opened; wake a waiting caller.
				select {
				case p.waitCh <- struct{}{}:
				default:
				}
				return nil, fmt.Errorf("pool: dial %s: %w", p.addr, err)
			}
			return conn, nil
		}

		// Pool exhausted. Block until Put, Close, or ctx cancellation.
		p.waiting++
		p.mu.Unlock()

		select {
		case <-p.waitCh:
			p.mu.Lock()
			p.waiting--
			p.mu.Unlock()
			// loop and retry; another goroutine may have raced us to the slot
		case <-ctx.Done():
			p.mu.Lock()
			p.waiting--
			p.mu.Unlock()
			return nil, ctx.Err()
		case <-p.closeCh:
			p.mu.Lock()
			p.waiting--
			p.mu.Unlock()
			return nil, ErrPoolClosed
		}
	}
}

// Put returns conn to the idle pool. If the pool is closed or the idle list
// is full, conn is closed and discarded instead. Callers must not use conn
// after calling Put.
func (p *Pool) Put(conn net.Conn) {
	if conn == nil {
		return
	}
	p.mu.Lock()
	if p.closed || len(p.idle) >= p.opts.MaxIdle {
		p.open--
		p.mu.Unlock()
		conn.Close()
		return
	}
	p.idle = append(p.idle, idleConn{conn: conn, idleSince: time.Now()})
	p.mu.Unlock()

	// Notify one waiting Get caller that a connection is available.
	select {
	case p.waitCh <- struct{}{}:
	default:
	}
}

// Close prevents new Get calls, closes all idle connections, and unblocks
// any goroutines currently waiting in Get.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.open -= len(idle)
	p.mu.Unlock()

	close(p.closeCh) // broadcast to all blocked Get callers
	for _, ic := range idle {
		ic.conn.Close()
	}
}

// Stats returns a snapshot of the pool's current state.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Open:    p.open,
		Idle:    len(p.idle),
		Waiting: p.waiting,
	}
}
```

`New` clamps `MaxIdle` to `MaxOpen` so the two limits are always consistent. `Get` is a loop because a woken waiter may find the slot already taken by a racing goroutine; it must go back to sleep or dial. `close(p.closeCh)` broadcasts to all blocked callers simultaneously — a channel send would wake only one.

### Exercise 2: Test the Pool

Create `pool_test.go`:

```go
package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// echoServer starts a loopback TCP server that echoes all data back to the
// sender. The returned closer shuts the listener down.
func echoServer(t *testing.T) (addr string, closer func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(conn, conn) //nolint:errcheck
				conn.Close()
			}()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestGetReusesIdleConnection(t *testing.T) {
	t.Parallel()
	addr, stop := echoServer(t)
	defer stop()

	p := New(addr, PoolOptions{MaxOpen: 5, MaxIdle: 5})
	defer p.Close()

	conn, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Put(conn)

	if s := p.Stats(); s.Idle != 1 {
		t.Fatalf("idle = %d after Put, want 1", s.Idle)
	}

	conn2, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Put(conn2)

	if conn2 != conn {
		t.Fatal("Get should have returned the idle connection, not dialled a new one")
	}
}

func TestGetBlocksUntilPut(t *testing.T) {
	t.Parallel()
	addr, stop := echoServer(t)
	defer stop()

	p := New(addr, PoolOptions{MaxOpen: 1, MaxIdle: 1})
	defer p.Close()

	conn, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		c, err := p.Get(ctx)
		if err != nil {
			done <- err
			return
		}
		p.Put(c)
		done <- nil
	}()

	time.Sleep(50 * time.Millisecond) // let the goroutine enter the wait
	p.Put(conn)                       // release the only slot

	if err := <-done; err != nil {
		t.Fatalf("blocked Get returned error after Put: %v", err)
	}
}

func TestGetCancelledByContext(t *testing.T) {
	t.Parallel()
	addr, stop := echoServer(t)
	defer stop()

	p := New(addr, PoolOptions{MaxOpen: 1, MaxIdle: 1})
	defer p.Close()

	// Hold the only slot so the second Get must block.
	conn, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Put(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	_, err = p.Get(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestGetAfterCloseErrors(t *testing.T) {
	t.Parallel()
	addr, stop := echoServer(t)
	defer stop()

	p := New(addr, PoolOptions{MaxOpen: 5, MaxIdle: 5})
	p.Close()

	_, err := p.Get(context.Background())
	if !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("err = %v, want ErrPoolClosed", err)
	}
}

func TestHealthCheckEvictsUnhealthyConnection(t *testing.T) {
	t.Parallel()
	addr, stop := echoServer(t)
	defer stop()

	callCount := 0
	p := New(addr, PoolOptions{
		MaxOpen: 5,
		MaxIdle: 5,
		HealthCheck: func(_ net.Conn) error {
			callCount++
			return errors.New("unhealthy")
		},
	})
	defer p.Close()

	conn, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Put(conn)

	// The next Get must run the health check on the idle conn, discard it,
	// then dial a fresh connection.
	conn2, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Put(conn2)

	if callCount == 0 {
		t.Fatal("health check was not called")
	}
	if conn2 == conn {
		t.Fatal("unhealthy connection must not be reused")
	}
}

func TestIdleTimeoutEvictsStaleConnection(t *testing.T) {
	t.Parallel()
	addr, stop := echoServer(t)
	defer stop()

	p := New(addr, PoolOptions{
		MaxOpen:     5,
		MaxIdle:     5,
		IdleTimeout: 10 * time.Millisecond,
	})
	defer p.Close()

	conn, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Put(conn)

	time.Sleep(30 * time.Millisecond) // exceed IdleTimeout

	conn2, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Put(conn2)

	if conn2 == conn {
		t.Fatal("stale idle connection should have been evicted and a fresh one dialled")
	}
}

func TestStatsMirrorsPoolState(t *testing.T) {
	t.Parallel()
	addr, stop := echoServer(t)
	defer stop()

	p := New(addr, PoolOptions{MaxOpen: 5, MaxIdle: 5})
	defer p.Close()

	if s := p.Stats(); s.Open != 0 || s.Idle != 0 {
		t.Fatalf("new pool stats = %+v, want all zero", s)
	}

	conn, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s := p.Stats(); s.Open != 1 || s.Idle != 0 {
		t.Fatalf("after Get: %+v, want open=1 idle=0", s)
	}

	p.Put(conn)
	if s := p.Stats(); s.Open != 1 || s.Idle != 1 {
		t.Fatalf("after Put: %+v, want open=1 idle=1", s)
	}
}

func TestCloseDrainsIdlePool(t *testing.T) {
	t.Parallel()
	addr, stop := echoServer(t)
	defer stop()

	p := New(addr, PoolOptions{MaxOpen: 5, MaxIdle: 5})

	conn, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Put(conn)

	p.Close()

	if s := p.Stats(); s.Open != 0 || s.Idle != 0 {
		t.Fatalf("after Close: %+v, want both zero", s)
	}
}

// ExampleNew shows that a freshly created pool makes no connections.
func ExampleNew() {
	p := New("127.0.0.1:9999", PoolOptions{MaxOpen: 3, MaxIdle: 2})
	defer p.Close()
	s := p.Stats()
	fmt.Printf("open=%d idle=%d waiting=%d\n", s.Open, s.Idle, s.Waiting)
	// Output: open=0 idle=0 waiting=0
}
```

Your turn: add `TestConcurrentGetPut` that launches 10 goroutines, each calling `Get` then `Put` 20 times on a pool with `MaxOpen: 3`. Assert that `p.Stats().Open` never exceeds 3 and that every `Get` returns `nil` error. Run with `-race` to confirm there are no data races.

### Exercise 3: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"example.com/pool"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(conn, conn) //nolint:errcheck
				conn.Close()
			}()
		}
	}()

	p := pool.New(ln.Addr().String(), pool.PoolOptions{
		MaxOpen:     5,
		MaxIdle:     3,
		IdleTimeout: 30 * time.Second,
	})
	defer p.Close()

	ctx := context.Background()

	conns := make([]net.Conn, 3)
	for i := range conns {
		conns[i], err = p.Get(ctx)
		if err != nil {
			log.Fatal(err)
		}
	}
	s := p.Stats()
	fmt.Printf("after 3 gets:  open=%d idle=%d\n", s.Open, s.Idle)

	for _, c := range conns {
		p.Put(c)
	}
	s = p.Stats()
	fmt.Printf("after 3 puts:  open=%d idle=%d\n", s.Open, s.Idle)

	c, err := p.Get(ctx)
	if err != nil {
		log.Fatal(err)
	}
	p.Put(c)
	s = p.Stats()
	fmt.Printf("after reuse:   open=%d idle=%d\n", s.Open, s.Idle)
}
```

Run with:

```bash
go run ./cmd/demo
```

Expected output:

```
after 3 gets:  open=3 idle=0
after 3 puts:  open=3 idle=3
after reuse:   open=3 idle=3
```

## Common Mistakes

### Putting a Broken Connection Back Into the Pool

Wrong: calling `p.Put(conn)` after a failed write or read. If the write failed, the connection may be half-closed; the next caller who receives it from `Get` will see an error on their first operation.

What happens: the pool silently recycles a broken connection, causing hard-to-diagnose errors in unrelated goroutines.

Fix: close and discard on any I/O error; never call `Put`:

```go
if _, err := conn.Write(req); err != nil {
	conn.Close() // discard; do NOT call p.Put
	return err
}
p.Put(conn)
```

### Not Signalling Waiters After a Failed Dial

Wrong: decrementing `p.open` after a dial failure but not sending to `waitCh`. Any goroutine blocked in `Get` stays blocked until its context expires, even though a slot is now free.

What happens: under load, a transient dial error cascades into context timeouts for all waiting callers.

Fix: send to `waitCh` after decrementing `open`:

```go
p.mu.Lock()
p.open--
p.mu.Unlock()
select {
case p.waitCh <- struct{}{}:
default:
}
```

### Using `close(ch)` vs Sending to Signal One Waiter

Wrong: calling `close(p.waitCh)` from `Put` to notify waiters on shutdown. A closed channel can be received from repeatedly and will panic on a subsequent send.

What happens: `Put` panics with "send on closed channel" if called after `Close`.

Fix: use `close` only for the dedicated `closeCh` that is never sent to, and use a non-blocking send on `waitCh` for per-connection notifications. The pool above separates these two channels precisely for this reason.

## Verification

From `~/go-exercises/pool`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Add `TestConcurrentGetPut` (see Exercise 2) before the final run and confirm the race detector reports nothing.

## Summary

- A pool tracks open count (total, server-facing) and idle count (reusable, client-facing) separately; enforce both limits.
- LIFO reuse keeps the working connection set small under light load.
- A buffered `chan struct{}` of capacity one is a correct and simple semaphore for blocking on pool exhaustion.
- A `select` on `waitCh`, `ctx.Done()`, and `closeCh` gives each blocked caller independent cancellation and a clean shutdown path.
- `close(closeCh)` broadcasts to all blocked callers simultaneously; a send would wake only one.
- On failed health check or idle timeout, discard the connection and continue the idle-scanning loop; do not return the broken connection to the caller.

## What's Next

Next: [TLS Server and Client](../08-tls-server-and-client/08-tls-server-and-client.md).

## Resources

- [database/sql: SetMaxOpenConns, SetMaxIdleConns, SetConnMaxIdleTime](https://pkg.go.dev/database/sql#DB.SetMaxOpenConns) — the production pool model this lesson mirrors
- [net/http: Transport](https://pkg.go.dev/net/http#Transport) — MaxIdleConns, MaxConnsPerHost, IdleConnTimeout fields
- [net: Conn interface and DialTimeout](https://pkg.go.dev/net#Conn) — the interface every pooled connection satisfies
- [sync: Mutex](https://pkg.go.dev/sync#Mutex) — the synchronization primitive used throughout
- [Go Blog: Concurrency Patterns — Context](https://go.dev/blog/context) — context cancellation and timeout propagation
