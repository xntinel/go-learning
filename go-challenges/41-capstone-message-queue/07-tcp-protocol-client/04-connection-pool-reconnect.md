# Exercise 4: Connection Pool with Reconnect and Backoff

Opening a TCP connection costs a handshake round trip, so dialing fresh per request throws that cost away every call. A connection pool keeps a bounded set of established connections idle between uses and hands them back out. The hard part is failure: a broken connection must be discarded, and a dead server must be re-dialed with exponential backoff and jitter so a fleet of reconnecting clients does not flood a recovering server. This exercise builds that pool.

This module is fully self-contained: it bundles its own minimal frame codec and in-order server, starts with its own `go mod init`, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
frame.go             Frame, Encode, Decode, APIProduce, RoundTrip (synchronous one-shot)
server.go            ServeConn: in-order echo loop for the demo and tests
pool.go              Pool, Backoff, Get/Put/Close, dialWithBackoff (jittered, ctx-aware)
cmd/
  demo/
    main.go          3 requests through the pool, all on one reused connection
mqpool_test.go       reuse over loopback, broken-conn discard, backoff retry, ctx cancel
```

- Files: `frame.go`, `server.go`, `pool.go`, `cmd/demo/main.go`, `mqpool_test.go`.
- Implement: `Pool` with `Get(ctx)`/`Put(conn, broken)`/`Close`, an injected `dial` so tests control failures, and `dialWithBackoff` with exponential backoff, full jitter, and context cancellation.
- Test: prove the pool reuses one connection across many requests, discards a broken one, retries a flaky dial until it succeeds, and gives up when the context expires.
- Verify: `go test -race ./...`

### Why bound the pool, and why backoff plus jitter on reconnect

A pool is two ideas in one struct: reuse and a bound. Reuse is the obvious win — an idle, already-handshaked connection skips the SYN/SYN-ACK/ACK round trip, so a workload of many small requests spends its time on requests instead of handshakes. The bound is the less obvious but equally important half. An unbounded pool under a burst opens connections until it hits the server's accept backlog or the client's file-descriptor limit, converting a load spike into a connection storm. Capping the idle set with a buffered channel of fixed size means `Get` reuses when it can and `Put` closes the surplus when the idle set is already full, so the pool never holds more than its bound idle and the surplus is reclaimed rather than leaked.

Failure is where the design earns its keep. Connections die in ways the holder only discovers on use: the server restarted, a load balancer recycled an idle socket, a NAT dropped the mapping. So `Put` takes a `broken bool` — the borrower, who just tried to use the connection, reports whether it failed — and a broken connection is closed instead of returned to the idle set. The next `Get` then finds the idle set empty and dials a replacement.

Dialing a replacement against a server that is itself down is the dangerous path. The naive loop, `for { if conn, err := dial(); err == nil { return conn } }`, spins as fast as the kernel can refuse connections, and when a whole fleet does it at once against a restarting server, the synchronized retry flood keeps the server pinned down — a self-inflicted denial of service. Exponential backoff fixes the rate: wait `Base` after the first failure, `Base*Factor` after the second, doubling up to a `Max` ceiling, so the load offered to a failing server falls geometrically. Jitter fixes the synchronization: instead of sleeping exactly the backoff delay, sleep a random duration in `[0, delay]` (full jitter), which spreads a fleet's retries across the interval so they stop hammering on the same tick. The AWS "exponential backoff and jitter" analysis shows full jitter minimizes both server contention and total time to recovery. Finally, the loop selects on `ctx.Done()` every iteration, so a caller can bound how long it will wait for a connection rather than blocking forever on an unreachable host.

Create `frame.go`:

```go
package mqpool

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// APIKey identifies the protocol operation carried in a Frame header.
type APIKey uint16

const APIProduce APIKey = 0

var (
	ErrBadFrame   = errors.New("malformed frame")
	ErrPoolClosed = errors.New("connection pool closed")
)

// headerLen is the fixed body prefix after the 4-byte length field.
const headerLen = 8

// Frame is one protocol message. length = headerLen + len(Payload).
type Frame struct {
	APIKey        APIKey
	APIVersion    uint16
	CorrelationID int32
	Payload       []byte
}

// Encode writes f to w as a complete, length-prefixed frame.
func (f *Frame) Encode(w io.Writer) error {
	length := headerLen + len(f.Payload)
	hdr := make([]byte, 4+headerLen)
	binary.BigEndian.PutUint32(hdr[0:], uint32(length))
	binary.BigEndian.PutUint16(hdr[4:], uint16(f.APIKey))
	binary.BigEndian.PutUint16(hdr[6:], f.APIVersion)
	binary.BigEndian.PutUint32(hdr[8:], uint32(f.CorrelationID))
	if _, err := w.Write(hdr); err != nil {
		return fmt.Errorf("mqpool: write header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("mqpool: write payload: %w", err)
		}
	}
	return nil
}

// Decode reads exactly one frame from r using io.ReadFull.
func Decode(r io.Reader) (*Frame, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("mqpool: read length: %w", err)
	}
	length := int(binary.BigEndian.Uint32(lenBuf[:]))
	if length < headerLen {
		return nil, fmt.Errorf("%w: length=%d", ErrBadFrame, length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("mqpool: read frame body: %w", err)
	}
	return &Frame{
		APIKey:        APIKey(binary.BigEndian.Uint16(body[0:])),
		APIVersion:    binary.BigEndian.Uint16(body[2:]),
		CorrelationID: int32(binary.BigEndian.Uint32(body[4:])),
		Payload:       body[headerLen:],
	}, nil
}

// RoundTrip sends req on conn and reads exactly one response. It is synchronous
// and intended for a connection borrowed from the pool for one request at a
// time.
func RoundTrip(conn net.Conn, req *Frame) (*Frame, error) {
	if err := req.Encode(conn); err != nil {
		return nil, err
	}
	return Decode(conn)
}
```

Create `server.go`:

```go
package mqpool

import (
	"bufio"
	"net"
)

// ServeConn reads frames from conn and replies in order. Used by the demo and
// tests as the broker side of a pooled connection.
func ServeConn(conn net.Conn, h func(*Frame) *Frame) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	for {
		req, err := Decode(br)
		if err != nil {
			return
		}
		resp := h(req)
		if resp == nil {
			continue
		}
		if err := resp.Encode(bw); err != nil {
			return
		}
		if err := bw.Flush(); err != nil {
			return
		}
	}
}
```

Create `pool.go`:

```go
package mqpool

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// Backoff configures exponential backoff with full jitter for reconnect.
type Backoff struct {
	Base   time.Duration // delay after the first failure
	Max    time.Duration // ceiling for the delay
	Factor float64       // multiplier per attempt
}

// next returns the delay for the following attempt, capped at Max.
func (b Backoff) next(d time.Duration) time.Duration {
	nd := time.Duration(float64(d) * b.Factor)
	if nd > b.Max {
		return b.Max
	}
	return nd
}

// jitter returns a random duration in [0, d] (full jitter). rand/v2's top-level
// functions are safe for concurrent use, so no locking is needed.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}

// Pool keeps a bounded set of idle connections and re-dials with backoff. A
// caller borrows a connection with Get and returns it with Put.
type Pool struct {
	dial    func(ctx context.Context) (net.Conn, error)
	idle    chan net.Conn
	Backoff Backoff

	mu     sync.Mutex
	closed bool
}

// NewPool creates a pool that keeps up to size idle connections and uses dial
// to open new ones. The default Backoff doubles from 50ms to a 5s ceiling.
func NewPool(size int, dial func(ctx context.Context) (net.Conn, error)) *Pool {
	if size < 1 {
		size = 1
	}
	return &Pool{
		dial: dial,
		idle: make(chan net.Conn, size),
		Backoff: Backoff{
			Base:   50 * time.Millisecond,
			Max:    5 * time.Second,
			Factor: 2,
		},
	}
}

// Get returns a connection: a reused idle one if available, otherwise a freshly
// dialed one (with backoff). It honors ctx for the dial wait.
func (p *Pool) Get(ctx context.Context) (net.Conn, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, ErrPoolClosed
	}

	select {
	case conn := <-p.idle:
		return conn, nil
	default:
	}
	return p.dialWithBackoff(ctx)
}

// Put returns conn to the idle set. If broken is true (the borrower's request
// failed), or the pool is closed, or the idle set is full, conn is closed.
func (p *Pool) Put(conn net.Conn, broken bool) {
	if conn == nil {
		return
	}
	if broken {
		conn.Close()
		return
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		conn.Close()
		return
	}
	select {
	case p.idle <- conn:
	default:
		conn.Close()
	}
}

// Close marks the pool closed and shuts every idle connection.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	for {
		select {
		case conn := <-p.idle:
			conn.Close()
		default:
			return nil
		}
	}
}

// dialWithBackoff retries p.dial with exponential backoff and full jitter,
// returning as soon as a dial succeeds or ctx is done.
func (p *Pool) dialWithBackoff(ctx context.Context) (net.Conn, error) {
	delay := p.Backoff.Base
	for {
		conn, err := p.dial(ctx)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("mqpool: dial: %w", ctx.Err())
		}
		select {
		case <-time.After(jitter(delay)):
		case <-ctx.Done():
			return nil, fmt.Errorf("mqpool: dial canceled: %w", ctx.Err())
		}
		delay = p.Backoff.next(delay)
	}
}
```

Trace the failure path. A borrower calls `Get`, receives a connection, runs `RoundTrip`, and the connection has gone bad — the write or read errors. The borrower calls `Put(conn, true)`, which closes the dead socket rather than poisoning the idle set with it. The next `Get` finds the idle channel empty and enters `dialWithBackoff`. If the server is back, the first dial succeeds and returns immediately. If it is still down, the loop sleeps a jittered fraction of `Base`, then `Base*Factor`, then doubles toward `Max`, checking `ctx.Done()` on every sleep so a caller with a deadline gets `context.DeadlineExceeded` instead of an unbounded wait. When the server recovers, whichever attempt lands first returns the connection, and because each client jittered independently they do not all reconnect on the same instant.

### The runnable demo

The demo starts an in-order echo server on a loopback port, builds a pool whose `dial` counts how many times it actually opens a socket, then sends three requests, each `Get` / `RoundTrip` / `Put`. The dial counter stays at 1: the first `Get` dials, every later `Get` reuses the connection `Put` returned.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync/atomic"

	"example.com/mqpool"
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
			go mqpool.ServeConn(conn, func(req *mqpool.Frame) *mqpool.Frame {
				return &mqpool.Frame{APIKey: req.APIKey, Payload: req.Payload}
			})
		}
	}()

	var dials atomic.Int64
	pool := mqpool.NewPool(4, func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	})
	defer pool.Close()

	for i := 1; i <= 3; i++ {
		conn, err := pool.Get(context.Background())
		if err != nil {
			log.Fatal(err)
		}
		payload := fmt.Appendf(nil, "ping-%d", i)
		resp, err := mqpool.RoundTrip(conn, &mqpool.Frame{APIKey: mqpool.APIProduce, Payload: payload})
		pool.Put(conn, err != nil)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("request %d: reply=%q (dials so far: %d)\n", i, resp.Payload, dials.Load())
	}
	fmt.Println("pool reused one connection across 3 requests")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request 1: reply="ping-1" (dials so far: 1)
request 2: reply="ping-2" (dials so far: 1)
request 3: reply="ping-3" (dials so far: 1)
pool reused one connection across 3 requests
```

### Tests

`TestPoolReusesConnection` runs five requests over a loopback listener and asserts the dial counter is 1 — the reuse the pool exists for. `TestPoolDiscardsBroken` returns a connection as broken and asserts the next `Get` dials again. `TestDialBackoffEventuallySucceeds` injects a dial that fails three times then succeeds and asserts exactly four attempts. `TestDialBackoffRespectsContext` injects an always-failing dial under a short deadline and asserts `context.DeadlineExceeded`. The backoff tests use a tiny `Base` so they finish in milliseconds.

Create `mqpool_test.go`:

```go
package mqpool

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func echo(req *Frame) *Frame {
	return &Frame{APIKey: req.APIKey, Payload: append([]byte(nil), req.Payload...)}
}

func startLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go ServeConn(conn, echo)
		}
	}()
	return ln
}

func TestPoolReusesConnection(t *testing.T) {
	t.Parallel()
	ln := startLoopback(t)

	var dials atomic.Int64
	p := NewPool(4, func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	})
	t.Cleanup(func() { p.Close() })

	for i := 0; i < 5; i++ {
		conn, err := p.Get(context.Background())
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		resp, err := RoundTrip(conn, &Frame{APIKey: APIProduce, Payload: []byte("hi")})
		p.Put(conn, err != nil)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		if !bytes.Equal(resp.Payload, []byte("hi")) {
			t.Errorf("payload = %q, want hi", resp.Payload)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Errorf("dials = %d, want 1 (connection reused)", got)
	}
}

func TestPoolDiscardsBroken(t *testing.T) {
	t.Parallel()
	ln := startLoopback(t)

	var dials atomic.Int64
	p := NewPool(4, func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	})
	t.Cleanup(func() { p.Close() })

	conn, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	p.Put(conn, true) // report broken; pool must discard it

	if _, err := p.Get(context.Background()); err != nil {
		t.Fatalf("Get after discard: %v", err)
	}
	if got := dials.Load(); got != 2 {
		t.Errorf("dials = %d, want 2 (broken conn re-dialed)", got)
	}
}

func TestDialBackoffEventuallySucceeds(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })

	var calls atomic.Int64
	p := NewPool(2, func(ctx context.Context) (net.Conn, error) {
		if calls.Add(1) <= 3 {
			return nil, errors.New("connection refused")
		}
		return c1, nil
	})
	p.Backoff = Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond, Factor: 2}

	conn, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if conn == nil {
		t.Fatal("Get returned nil conn")
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("dial attempts = %d, want 4", got)
	}
}

func TestDialBackoffRespectsContext(t *testing.T) {
	t.Parallel()
	p := NewPool(1, func(ctx context.Context) (net.Conn, error) {
		return nil, errors.New("connection refused")
	})
	p.Backoff = Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond, Factor: 2}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if _, err := p.Get(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}
```

## Review

The pool is correct when reuse, the bound, and the backoff all hold. Confirm that `Get` reuses an idle connection before dialing and `Put` closes a connection reported broken instead of returning it, so a dead socket never re-enters circulation — the reuse and discard tests pin both. Confirm `dialWithBackoff` grows the delay geometrically, jitters each sleep, and returns `ctx.Err()` promptly on cancellation; the four-attempt test fixes the retry count and the deadline test fixes the give-up behavior. Run under `-race`: the dial counter and the idle channel are touched from multiple goroutines, and the `atomic.Int64` plus the channel are what keep that safe.

Common mistakes for this feature. The first is a retry loop with no backoff, which turns a fleet of reconnecting clients into a flood that keeps a recovering server down; the geometric delay is the throttle. The second is backoff with no jitter, which fixes the rate but not the synchronization, so every client still retries on the same tick — full jitter spreads them. The third is a backoff loop that ignores its context and blocks forever on an unreachable host; selecting on `ctx.Done()` each iteration is what lets a caller bound the wait. The fourth is returning a broken connection to the idle set, which hands the next borrower a guaranteed failure.

## Resources

- [`net.Dialer.DialContext`](https://pkg.go.dev/net#Dialer.DialContext) — the context-aware dial the pool's injected `dial` wraps, so a deadline cancels an in-progress connect.
- [Exponential Backoff And Jitter (AWS Architecture Blog)](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — the analysis showing full jitter minimizes contention and recovery time for a reconnecting fleet.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — the concurrency-safe `Int64N` backing the jitter, usable from multiple dialing goroutines without a lock.

---

Back to [03-request-pipelining.md](03-request-pipelining.md) | Next: [../08-full-message-queue/00-concepts.md](../08-full-message-queue/00-concepts.md)
