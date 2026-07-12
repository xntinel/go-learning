# Exercise 1: L4 TCP Proxy

An L4 proxy accepts a downstream TCP connection, dials an upstream backend, and copies bytes both ways without ever looking at the payload. This exercise builds the whole thing as one self-contained package: connection records and sentinel errors, functional options and a constructor, a `Serve` loop with a built-in drain phase, and the half-close copy that is the heart of correct TCP proxying.

This module is fully self-contained: its own `go mod init`, every type it needs defined inline (including the round-robin balancer it embeds), its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
proxy.go            ConnState, ConnRecord, Stats, Proxy, options, New,
                    roundRobin, Serve, handle, copyHalfClose, addConn/removeConn
cmd/
  demo/
    main.go         echo server + proxy in-process, send a message, print stats
proxy_test.go       state strings, option validation, round-robin, bidirectional
                    forward, byte stats, idle timeout, max-conns drop, drain,
                    multiple upstreams
```

- Files: `proxy.go`, `cmd/demo/main.go`, `proxy_test.go`.
- Implement: `Proxy` with `New(opts ...Option)`, `Serve(ctx, ln)`, `Stats()`, and `Conns()`; functional options `WithUpstreams`, `WithMaxConns`, `WithIdleTimeout`; the internal `roundRobin` balancer and the `copyHalfClose` primitive.
- Test: `ConnState.String`, option validation, round-robin distribution and concurrency, bidirectional forwarding through a real loopback echo server, byte accounting, idle-timeout close, max-conns drop, graceful drain on context cancel, and round-robin across multiple upstreams.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

This is a library package. Verify it with `go test`, not `go run` — though `cmd/demo` is runnable to see it work end to end.

### The shape of the package

The proxy is a `package proxy`, not a `main`, so it can be embedded in a larger data plane and exercised by a deterministic test suite. Four pieces fit together.

The data types come first. `ConnState` is a tiny enum (`active`, `draining`, `closed`) with a `String` method so logs and the admin surface read cleanly. `ConnRecord` is the per-connection observability row — client and upstream address, bytes each way, start time, state. `Stats` is the aggregate snapshot. `ErrNoUpstreams` is the one sentinel error, returned when a caller configures zero upstreams.

The constructor uses the functional-options pattern. `New` seeds sensible defaults (one local upstream, 1000 max connections, a 30-second idle timeout) and then applies each `Option`, any of which may reject its input. `WithUpstreams` rejects an empty list with `ErrNoUpstreams`; `WithMaxConns` rejects a value below one. This keeps `New`'s signature stable as configuration grows and makes every invalid configuration a construction-time error rather than a runtime surprise.

`Serve` is the accept loop. A background goroutine closes the listener when the context is cancelled, which unblocks `Accept`. The loop checks the active-connection count against `maxConns` and drops excess connections immediately. For each accepted connection it calls `handlers.Add(1)` in the loop body — in the parent goroutine, before spawning the handler — then starts the handler. `defer p.handlers.Wait()` at the top of `Serve` is the drain: `Serve` cannot return until every handler has finished, so cancelling the context closes the listener and then waits out the in-flight traffic.

`handle` is one connection's lifecycle. It dials the next upstream with a timeout, registers a `ConnRecord`, bumps the counters, sets a single idle deadline on both sides, then runs the two copy goroutines and waits. `copyHalfClose` is the critical building block: it runs `io.Copy` and, when the source reaches EOF, calls `CloseWrite()` on the destination so the peer receives a FIN and learns this direction is done while the other direction keeps flowing. If the destination is not a `*net.TCPConn` it falls back to `Close()`.

Create `proxy.go`:

```go
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ConnState describes the lifecycle phase of a proxied connection.
type ConnState int8

const (
	StateActive   ConnState = iota // actively forwarding traffic
	StateDraining                  // shutdown signalled; finishing in-flight data
	StateClosed                    // both directions have finished
)

// String implements fmt.Stringer.
func (s ConnState) String() string {
	switch s {
	case StateActive:
		return "active"
	case StateDraining:
		return "draining"
	case StateClosed:
		return "closed"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// ConnRecord holds per-connection observability data.
type ConnRecord struct {
	ID           uint64
	ClientAddr   string
	UpstreamAddr string
	BytesSent    int64 // downstream -> upstream
	BytesRecv    int64 // upstream -> downstream
	StartedAt    time.Time
	State        ConnState
}

// Stats holds aggregate counters for a Proxy.
type Stats struct {
	TotalAccepted int64
	ActiveConns   int64
	BytesSent     int64
	BytesRecv     int64
	Errors        int64
}

// ErrNoUpstreams is returned by New when no upstream addresses are provided.
var ErrNoUpstreams = errors.New("proxy: no upstream addresses configured")

// Option configures a Proxy.
type Option func(*Proxy) error

// Proxy is a layer-4 TCP proxy. The zero value is not usable; call New.
type Proxy struct {
	upstreams   []string
	maxConns    int
	idleTimeout time.Duration

	mu     sync.RWMutex
	conns  map[uint64]*ConnRecord
	nextID uint64

	totalAccepted atomic.Int64
	activeConns   atomic.Int64
	bytesSent     atomic.Int64
	bytesRecv     atomic.Int64
	errCount      atomic.Int64

	rr       *roundRobin
	handlers sync.WaitGroup
}

// WithUpstreams sets the upstream backend addresses (at least one required).
func WithUpstreams(addrs ...string) Option {
	return func(p *Proxy) error {
		if len(addrs) == 0 {
			return ErrNoUpstreams
		}
		p.upstreams = addrs
		return nil
	}
}

// WithMaxConns sets the maximum number of concurrent proxied connections.
// Incoming connections are dropped when the limit is reached.
func WithMaxConns(n int) Option {
	return func(p *Proxy) error {
		if n < 1 {
			return fmt.Errorf("proxy: max conns must be >= 1, got %d", n)
		}
		p.maxConns = n
		return nil
	}
}

// WithIdleTimeout sets the per-connection idle deadline.
// A zero or negative value disables the deadline.
func WithIdleTimeout(d time.Duration) Option {
	return func(p *Proxy) error {
		p.idleTimeout = d
		return nil
	}
}

// New creates a Proxy with the given options.
// Defaults: upstreams=["127.0.0.1:8080"], maxConns=1000, idleTimeout=30s.
func New(opts ...Option) (*Proxy, error) {
	p := &Proxy{
		upstreams:   []string{"127.0.0.1:8080"},
		maxConns:    1000,
		idleTimeout: 30 * time.Second,
		conns:       make(map[uint64]*ConnRecord),
	}
	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}
	p.rr = newRoundRobin(p.upstreams)
	return p, nil
}

// roundRobin distributes connections across a fixed set of backend addresses.
// The index is protected by a mutex; an atomic index with (idx % len) is a
// check-then-act race that the mutex avoids.
type roundRobin struct {
	addrs []string
	mu    sync.Mutex
	idx   int
}

func newRoundRobin(addrs []string) *roundRobin {
	return &roundRobin{addrs: addrs}
}

func (rr *roundRobin) next() string {
	rr.mu.Lock()
	addr := rr.addrs[rr.idx%len(rr.addrs)]
	rr.idx++
	rr.mu.Unlock()
	return addr
}

// Serve accepts connections from ln and proxies them to the configured
// upstreams until ctx is cancelled. It waits for all active handlers to
// finish before returning, providing a built-in drain phase.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	defer p.handlers.Wait()

	for {
		downstream, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("proxy: accept: %w", err)
		}
		if int(p.activeConns.Load()) >= p.maxConns {
			downstream.Close()
			p.errCount.Add(1)
			continue
		}
		p.handlers.Add(1)
		go func() {
			defer p.handlers.Done()
			p.handle(downstream)
		}()
	}
}

// Stats returns a snapshot of aggregate proxy counters.
func (p *Proxy) Stats() Stats {
	return Stats{
		TotalAccepted: p.totalAccepted.Load(),
		ActiveConns:   p.activeConns.Load(),
		BytesSent:     p.bytesSent.Load(),
		BytesRecv:     p.bytesRecv.Load(),
		Errors:        p.errCount.Load(),
	}
}

// Conns returns a snapshot of all currently tracked connection records.
func (p *Proxy) Conns() []ConnRecord {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]ConnRecord, 0, len(p.conns))
	for _, rec := range p.conns {
		out = append(out, *rec)
	}
	return out
}

func (p *Proxy) handle(downstream net.Conn) {
	defer downstream.Close()

	upstream, err := net.DialTimeout("tcp", p.rr.next(), 5*time.Second)
	if err != nil {
		p.errCount.Add(1)
		return
	}
	defer upstream.Close()

	id := p.addConn(downstream.RemoteAddr().String(), upstream.RemoteAddr().String())
	defer p.removeConn(id)

	p.totalAccepted.Add(1)
	p.activeConns.Add(1)
	defer p.activeConns.Add(-1)

	if p.idleTimeout > 0 {
		deadline := time.Now().Add(p.idleTimeout)
		downstream.SetDeadline(deadline) //nolint:errcheck
		upstream.SetDeadline(deadline)   //nolint:errcheck
	}

	var (
		sent int64
		recv int64
		wg   sync.WaitGroup
	)
	wg.Add(2)

	// downstream -> upstream
	go func() {
		defer wg.Done()
		sent, _ = copyHalfClose(upstream, downstream)
		p.bytesSent.Add(sent)
	}()

	// upstream -> downstream
	go func() {
		defer wg.Done()
		recv, _ = copyHalfClose(downstream, upstream)
		p.bytesRecv.Add(recv)
	}()

	wg.Wait()
	// wg.Wait() provides the happens-before guarantee that makes reading
	// sent and recv here race-free: each goroutine writes exactly one
	// variable and wg.Done() is the synchronisation point.

	p.mu.Lock()
	if rec, ok := p.conns[id]; ok {
		rec.BytesSent = sent
		rec.BytesRecv = recv
		rec.State = StateClosed
	}
	p.mu.Unlock()
}

func (p *Proxy) addConn(clientAddr, upstreamAddr string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	id := p.nextID
	p.conns[id] = &ConnRecord{
		ID:           id,
		ClientAddr:   clientAddr,
		UpstreamAddr: upstreamAddr,
		StartedAt:    time.Now(),
		State:        StateActive,
	}
	return id
}

func (p *Proxy) removeConn(id uint64) {
	p.mu.Lock()
	delete(p.conns, id)
	p.mu.Unlock()
}

// copyHalfClose copies from src to dst using io.Copy. When src signals EOF
// it closes the write half of dst so that dst's peer receives FIN and knows
// no more data is coming from this direction, while the other direction can
// still deliver buffered data. If dst is not a *net.TCPConn (e.g. net.Pipe
// connections in tests), it falls back to Close on dst.
func copyHalfClose(dst, src net.Conn) (int64, error) {
	n, err := io.Copy(dst, src)
	if tc, ok := dst.(*net.TCPConn); ok {
		tc.CloseWrite() //nolint:errcheck
	} else {
		dst.Close()
	}
	return n, err
}
```

Read `Serve` and `handle` together. `Serve` owns the listener and the wait group; `handle` owns one connection and its two copy goroutines. The deadline is set once, on both sides, before the goroutines start, so a connection that goes quiet for `idleTimeout` has both reads fail together and the handler unwinds cleanly through the deferred closes. The final `mu.Lock()` block records the byte totals and flips the record to `StateClosed` before `removeConn` deletes it — a real admin surface would snapshot the record before deletion; here the deletion keeps the table bounded to live connections.

### The runnable demo

The demo starts an in-process echo server as the upstream, points a proxy at it, sends one message through the proxy, reads the echo back, and prints the aggregate stats. The message is nineteen bytes, so the byte counts are deterministic; the listener addresses are not printed, keeping the output stable.

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

	proxy "example.com/l4proxy"
)

func main() {
	// Start an in-process echo server as the upstream backend.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer echoLn.Close()
	go runEchoServer(echoLn)

	// Create the proxy pointing at the echo server.
	p, err := proxy.New(
		proxy.WithUpstreams(echoLn.Addr().String()),
		proxy.WithMaxConns(100),
		proxy.WithIdleTimeout(5*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := p.Serve(ctx, proxyLn); err != nil {
			log.Printf("proxy stopped: %v", err)
		}
	}()

	// Connect to the proxy and verify the echo.
	conn, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	msg := "hello, service mesh"
	if _, err := io.WriteString(conn, msg); err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("sent:     %q\n", msg)
	fmt.Printf("received: %q\n", string(buf))

	conn.Close()
	time.Sleep(50 * time.Millisecond)

	s := p.Stats()
	fmt.Printf("stats: accepted=%d active=%d sent=%d recv=%d errors=%d\n",
		s.TotalAccepted, s.ActiveConns, s.BytesSent, s.BytesRecv, s.Errors)
}

func runEchoServer(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			io.Copy(c, c) //nolint:errcheck
		}(c)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sent:     "hello, service mesh"
received: "hello, service mesh"
stats: accepted=1 active=0 sent=19 recv=19 errors=0
```

### Tests

The tests open real loopback TCP connections — they need an actual TCP stack but no external services. Two helpers carry the weight: `startEchoServer` runs an echo backend on `127.0.0.1:0` and registers its cleanup, and `startProxy` serves a proxy on its own loopback listener and cancels its context on cleanup. From there the cases pin each property: state strings, option validation, round-robin distribution and concurrency, bidirectional forwarding, byte accounting, the idle timeout closing a silent connection, the max-conns limit dropping the second connection, the graceful-shutdown return on context cancel, and round-robin spreading four connections across two upstreams.

Create `proxy_test.go`:

```go
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestConnStateString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state ConnState
		want  string
	}{
		{StateActive, "active"},
		{StateDraining, "draining"},
		{StateClosed, "closed"},
		{ConnState(99), "unknown(99)"},
	}
	for _, tc := range cases {
		got := tc.state.String()
		if got != tc.want {
			t.Errorf("ConnState(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()

	p, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if p.maxConns != 1000 {
		t.Errorf("maxConns = %d, want 1000", p.maxConns)
	}
	if p.idleTimeout != 30*time.Second {
		t.Errorf("idleTimeout = %v, want 30s", p.idleTimeout)
	}
	if len(p.upstreams) != 1 || p.upstreams[0] != "127.0.0.1:8080" {
		t.Errorf("upstreams = %v, want [127.0.0.1:8080]", p.upstreams)
	}
}

func TestNewRejectsEmptyUpstreams(t *testing.T) {
	t.Parallel()

	_, err := New(WithUpstreams())
	if !errors.Is(err, ErrNoUpstreams) {
		t.Fatalf("err = %v, want ErrNoUpstreams", err)
	}
}

func TestNewRejectsZeroMaxConns(t *testing.T) {
	t.Parallel()

	_, err := New(WithMaxConns(0))
	if err == nil {
		t.Fatal("expected error for maxConns=0")
	}
}

func TestRoundRobinDistributes(t *testing.T) {
	t.Parallel()

	rr := newRoundRobin([]string{"a:1", "b:2"})
	got := make(map[string]int)
	for i := 0; i < 4; i++ {
		got[rr.next()]++
	}
	if got["a:1"] != 2 || got["b:2"] != 2 {
		t.Errorf("distribution = %v, want a:1=2 b:2=2", got)
	}
}

func TestRoundRobinConcurrent(t *testing.T) {
	t.Parallel()

	rr := newRoundRobin([]string{"x:1", "y:2", "z:3"})
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rr.next()
		}()
	}
	wg.Wait() // must not race; run with -race
}

// startEchoServer starts a TCP echo server on 127.0.0.1:0 and returns its
// address. The server is automatically closed when t ends.
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c) //nolint:errcheck
			}(c)
		}
	}()
	return ln.Addr().String()
}

func startProxy(t *testing.T, p *Proxy) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		ln.Close()
	})
	go p.Serve(ctx, ln) //nolint:errcheck
	return ln.Addr().String()
}

func TestProxyForwardsBidirectional(t *testing.T) {
	t.Parallel()

	echoAddr := startEchoServer(t)
	p, err := New(WithUpstreams(echoAddr), WithIdleTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := startProxy(t, p)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	const payload = "hello proxy"
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("got %q, want %q", string(buf), payload)
	}
}

func TestProxyStatsCountBytes(t *testing.T) {
	t.Parallel()

	echoAddr := startEchoServer(t)
	p, err := New(WithUpstreams(echoAddr), WithIdleTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := startProxy(t, p)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}

	const payload = "stats"
	io.WriteString(conn, payload) //nolint:errcheck
	buf := make([]byte, len(payload))
	io.ReadFull(conn, buf) //nolint:errcheck
	conn.Close()

	// Allow the handler goroutine to record final stats.
	time.Sleep(50 * time.Millisecond)

	s := p.Stats()
	if s.TotalAccepted < 1 {
		t.Errorf("TotalAccepted = %d, want >= 1", s.TotalAccepted)
	}
	if s.BytesSent < int64(len(payload)) {
		t.Errorf("BytesSent = %d, want >= %d", s.BytesSent, len(payload))
	}
}

func TestProxyIdleTimeout(t *testing.T) {
	t.Parallel()

	echoAddr := startEchoServer(t)
	p, err := New(WithUpstreams(echoAddr), WithIdleTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := startProxy(t, p)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Send nothing; the idle deadline should close the proxy-side connection.
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected read error after idle timeout, got nil")
	}
}

func TestProxyMaxConnsDropsExcess(t *testing.T) {
	t.Parallel()

	// Blocking upstream: holds connections open.
	blockLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blockLn.Close() })
	go func() {
		for {
			c, err := blockLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(io.Discard, c) //nolint:errcheck
			}(c)
		}
	}()

	p, err := New(WithUpstreams(blockLn.Addr().String()), WithMaxConns(1))
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := startProxy(t, p)

	c1, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()

	// Give the proxy time to register the first connection.
	time.Sleep(50 * time.Millisecond)

	// Second connection: proxy is at its limit; it must be closed immediately.
	c2, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	c2.SetReadDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
	buf := make([]byte, 1)
	_, err = c2.Read(buf)
	if err == nil {
		t.Fatal("expected second connection to be dropped, got read success")
	}
}

func TestProxyGracefulShutdown(t *testing.T) {
	t.Parallel()

	echoAddr := startEchoServer(t)
	p, err := New(WithUpstreams(echoAddr))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- p.Serve(ctx, ln)
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}
}

func TestProxyMultipleUpstreams(t *testing.T) {
	t.Parallel()

	echoA := startEchoServer(t)
	echoB := startEchoServer(t)
	p, err := New(WithUpstreams(echoA, echoB), WithIdleTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := startProxy(t, p)

	// Open four connections and keep them open so their records stay live in
	// the tracking table while we snapshot it.
	for i := 0; i < 4; i++ {
		c, err := net.Dial("tcp", proxyAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}

	// Let the proxy register all four handlers and their upstream addresses.
	time.Sleep(100 * time.Millisecond)

	seen := make(map[string]int)
	for _, rec := range p.Conns() {
		seen[rec.UpstreamAddr]++
	}
	if seen[echoA] == 0 || seen[echoB] == 0 {
		t.Fatalf("round-robin missed an upstream: %v (echoA=%s echoB=%s)", seen, echoA, echoB)
	}
}

// ExampleConnState_String is auto-verified by go test.
func ExampleConnState_String() {
	fmt.Println(StateActive.String())
	fmt.Println(StateDraining.String())
	fmt.Println(StateClosed.String())
	// Output:
	// active
	// draining
	// closed
}
```

## Review

The forwarding path is sound when `copyHalfClose` calls `CloseWrite()` on a `*net.TCPConn` rather than `Close()`: the wrong choice sends RST instead of FIN and discards in-flight data, and `TestProxyForwardsBidirectional` is the guard that a full request and its echo survive both copy directions. The idle timeout is sound when the deadline is set on the accepted connection — not the listener — so a silent connection is closed; `TestProxyIdleTimeout` sends nothing and asserts the client read fails. The drain is sound when `handlers.Add(1)` runs in the accept loop before the handler starts and `Serve` defers `handlers.Wait()`; `TestProxyGracefulShutdown` asserts `Serve` returns `nil` after the context is cancelled. The max-conns guard is sound when the count is checked before spawning the handler; `TestProxyMaxConnsDropsExcess` holds one connection open against a blocking upstream and asserts the second is dropped. The whole suite holding under `go test -race` is the real bar: the race detector is what proves the two copy goroutines, the atomic counters, and the `RWMutex`-guarded tracking map genuinely interleave without a data race, and that reading `sent`/`recv` after `wg.Wait()` is safe.

## Resources

- [pkg.go.dev/net](https://pkg.go.dev/net) — `TCPConn.CloseWrite`, `SetDeadline`, `Listener`, `DialTimeout`, `Listen`; the authoritative source for every network API used here.
- [pkg.go.dev/io#Copy](https://pkg.go.dev/io#Copy) — `io.Copy` behavior on EOF and errors; the contract that it returns `nil` (not `io.EOF`) on a clean end-of-stream.
- [pkg.go.dev/sync#WaitGroup](https://pkg.go.dev/sync#WaitGroup) — the happens-before guarantee that makes reading the byte counters after `Wait()` race-free, and the `Add`-in-parent / `Done`-in-child drain pattern.
- [pkg.go.dev/sync/atomic#Int64](https://pkg.go.dev/sync/atomic#Int64) — the typed atomic counters used for aggregate stats without holding the map lock.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-round-robin-balancer.md](02-round-robin-balancer.md)
