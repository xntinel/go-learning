# 21. TCP Load Balancer

An L4 TCP load balancer accepts a client connection, selects a backend from a pool, dials it, and relays bytes bidirectionally — all without understanding the application protocol. The hard parts are algorithm selection under concurrent load, half-close propagation so neither end hangs waiting for EOF, atomic health state shared between the accept loop and health checks, and clean shutdown that drains active relays.

```text
lb/
  go.mod
  lb.go
  lb_test.go
  cmd/demo/main.go
```

## Concepts

### L4 vs L7 Balancing

An L4 (transport-layer) balancer operates on TCP connections: it sees source/destination IP and port, picks a backend, and copies raw bytes. It is simpler and faster than an L7 balancer, which must parse HTTP/gRPC/etc., but it cannot make routing decisions based on URL paths or headers.

The standard library's `net.Listener` and `net.Conn` are all you need. There is no HTTP stack involved.

### Selection Algorithms

**Round-robin**: maintain a monotonically incrementing index modulo the number of healthy backends. Provides even distribution when backends are homogeneous.

**Least-connections**: pick the healthy backend whose active connection count is lowest. Better for heterogeneous backends or long-lived connections where round-robin can pile connections on a slow backend. The active count must be decremented on disconnect, not only incremented on connect.

Both algorithms must exclude unhealthy backends from selection without holding a lock for the entire dial-and-relay cycle. Reads of the pool happen under a read lock; the algorithm operates on the copy.

### Connection Relay and Half-Close

Bidirectional relay requires two goroutines: one copying client-to-backend, one copying backend-to-client. Each `io.Copy` blocks until its source returns EOF or an error.

The critical detail is half-close: when the client sends FIN (write-side close), `io.Copy` on that goroutine returns, but the reverse goroutine is still running. Without intervention it blocks forever. The fix is to call `dst.Close()` after each `io.Copy` returns — closing the destination unblocks the reverse copy on the other goroutine by making its source return an error.

### Health State and Concurrency

`sync/atomic.Bool` (Go 1.19+) and `sync/atomic.Int64` carry health status and connection counts without locks. Multiple goroutines read and write these fields concurrently; atomic types make those operations safe and avoid false sharing compared to a coarse mutex.

A backend is marked unhealthy when a dial attempt fails. Restoring it to healthy requires active health checks (periodic dials) or manual intervention; passive marking alone creates a permanent failure. The implementation below exposes `SetHealthy` to let tests and health-check goroutines override the state.

## Exercises

### Exercise 1: Backend Pool and Balancing Algorithms

Create `lb.go`:

```go
package lb

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// ErrNoBackends is returned when all backends are unhealthy.
var ErrNoBackends = errors.New("lb: no healthy backends available")

// ErrInvalidWeight is returned when a backend weight is less than 1.
var ErrInvalidWeight = errors.New("lb: backend weight must be at least 1")

// ErrEmptyAddress is returned when a backend address is empty.
var ErrEmptyAddress = errors.New("lb: backend address must not be empty")

// Backend holds the address and runtime state of a single upstream server.
// It is always used as a pointer; the atomic fields must not be copied.
type Backend struct {
	addr    string
	weight  int
	healthy atomic.Bool
	active  atomic.Int64
	total   atomic.Int64
}

// NewBackend creates a healthy Backend with the given address and weight.
// weight must be >= 1; it is reserved for weighted round-robin extensions.
func NewBackend(addr string, weight int) (*Backend, error) {
	if addr == "" {
		return nil, ErrEmptyAddress
	}
	if weight < 1 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidWeight, weight)
	}
	b := &Backend{addr: addr, weight: weight}
	b.healthy.Store(true)
	return b, nil
}

// Addr returns the backend's network address.
func (b *Backend) Addr() string { return b.addr }

// Healthy reports whether the backend is currently marked healthy.
func (b *Backend) Healthy() bool { return b.healthy.Load() }

// Active returns the number of connections currently being relayed.
func (b *Backend) Active() int64 { return b.active.Load() }

// Total returns the lifetime count of connections routed to this backend.
func (b *Backend) Total() int64 { return b.total.Load() }

// Algorithm selects a backend from a pool for an incoming connection.
type Algorithm interface {
	Select(backends []*Backend) (*Backend, error)
}

// healthySubset returns only the backends currently marked healthy.
// It allocates a new slice so the caller can use it without holding a lock.
func healthySubset(backends []*Backend) []*Backend {
	out := make([]*Backend, 0, len(backends))
	for _, b := range backends {
		if b.Healthy() {
			out = append(out, b)
		}
	}
	return out
}

// RoundRobin distributes connections evenly across healthy backends in order.
// The index wraps at len(healthy), so adding or removing backends at runtime
// shifts which backend is "next" — this is expected and acceptable for L4.
type RoundRobin struct {
	mu  sync.Mutex
	idx int
}

// Select picks the next healthy backend in round-robin order.
func (r *RoundRobin) Select(backends []*Backend) (*Backend, error) {
	healthy := healthySubset(backends)
	if len(healthy) == 0 {
		return nil, ErrNoBackends
	}
	r.mu.Lock()
	b := healthy[r.idx%len(healthy)]
	r.idx++
	r.mu.Unlock()
	return b, nil
}

// LeastConnections routes each connection to the backend with the fewest
// active relays. It reads Active() without a lock; the atomic read is
// point-in-time and sufficient for this use case.
type LeastConnections struct{}

// Select picks the healthy backend whose Active() count is lowest.
func (l *LeastConnections) Select(backends []*Backend) (*Backend, error) {
	healthy := healthySubset(backends)
	if len(healthy) == 0 {
		return nil, ErrNoBackends
	}
	best := healthy[0]
	for _, b := range healthy[1:] {
		if b.Active() < best.Active() {
			best = b
		}
	}
	return best, nil
}

// LoadBalancer accepts TCP connections and relays each to a selected backend.
type LoadBalancer struct {
	alg      Algorithm
	backends []*Backend
	mu       sync.RWMutex
	listener net.Listener
	once     sync.Once
	done     chan struct{}
	wg       sync.WaitGroup
}

// New creates a LoadBalancer with the given algorithm and backend pool.
// At least one backend is required.
func New(alg Algorithm, backends ...*Backend) (*LoadBalancer, error) {
	if alg == nil {
		return nil, errors.New("lb: algorithm must not be nil")
	}
	if len(backends) == 0 {
		return nil, ErrNoBackends
	}
	return &LoadBalancer{
		alg:      alg,
		backends: backends,
		done:     make(chan struct{}),
	}, nil
}

// Listen starts accepting TCP connections on addr.
// Pass "127.0.0.1:0" to let the OS assign a port; use Addr() to retrieve it.
func (lb *LoadBalancer) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("lb: listen %s: %w", addr, err)
	}
	lb.listener = ln
	lb.wg.Add(1)
	go func() {
		defer lb.wg.Done()
		lb.acceptLoop(ln)
	}()
	return nil
}

// Addr returns the local address the load balancer is listening on,
// or nil if Listen has not been called.
func (lb *LoadBalancer) Addr() net.Addr {
	if lb.listener == nil {
		return nil
	}
	return lb.listener.Addr()
}

func (lb *LoadBalancer) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed by Close()
		}
		lb.wg.Add(1)
		go func() {
			defer lb.wg.Done()
			lb.handleConn(conn)
		}()
	}
}

func (lb *LoadBalancer) handleConn(client net.Conn) {
	defer client.Close()

	lb.mu.RLock()
	backends := lb.backends
	lb.mu.RUnlock()

	backend, err := lb.alg.Select(backends)
	if err != nil {
		return
	}

	upstream, err := net.Dial("tcp", backend.addr)
	if err != nil {
		// Passive health: mark unhealthy on dial failure.
		backend.healthy.Store(false)
		return
	}
	defer upstream.Close()

	backend.active.Add(1)
	backend.total.Add(1)
	defer func() { backend.active.Add(-1) }()

	relay(client, upstream)
}

// relay copies data bidirectionally between a and b.
// When one direction reaches EOF, it closes the destination of that half,
// which unblocks the io.Copy on the reverse goroutine.
func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		dst.Close()
	}
	wg.Add(2)
	go copyHalf(b, a)
	go copyHalf(a, b)
	wg.Wait()
}

// Close stops accepting new connections and waits for all active relays to finish.
// It is safe to call Close concurrently or multiple times.
func (lb *LoadBalancer) Close() error {
	var closeErr error
	lb.once.Do(func() {
		close(lb.done)
		if lb.listener != nil {
			closeErr = lb.listener.Close()
		}
	})
	lb.wg.Wait()
	return closeErr
}

// SetHealthy overrides the health state of the backend at the given address.
// Used by external health-check goroutines and tests.
func (lb *LoadBalancer) SetHealthy(addr string, healthy bool) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	for _, b := range lb.backends {
		if b.addr == addr {
			b.healthy.Store(healthy)
			return
		}
	}
}
```

### Exercise 2: Test the Load Balancer

Create `lb_test.go`:

```go
package lb

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

// --- unit tests: Backend construction ---

func TestNewBackendRejectsEmptyAddress(t *testing.T) {
	t.Parallel()
	_, err := NewBackend("", 1)
	if !errors.Is(err, ErrEmptyAddress) {
		t.Fatalf("err = %v, want ErrEmptyAddress", err)
	}
}

func TestNewBackendRejectsZeroWeight(t *testing.T) {
	t.Parallel()
	for _, w := range []int{0, -1, -100} {
		_, err := NewBackend("127.0.0.1:9000", w)
		if !errors.Is(err, ErrInvalidWeight) {
			t.Errorf("weight %d: err = %v, want ErrInvalidWeight", w, err)
		}
	}
}

func TestNewBackendStartsHealthy(t *testing.T) {
	t.Parallel()
	b, err := NewBackend("127.0.0.1:9000", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Healthy() {
		t.Fatal("new backend should start healthy")
	}
}

// --- unit tests: RoundRobin ---

func TestRoundRobinSelectsInOrder(t *testing.T) {
	t.Parallel()
	b0, _ := NewBackend("127.0.0.1:9000", 1)
	b1, _ := NewBackend("127.0.0.1:9001", 1)
	b2, _ := NewBackend("127.0.0.1:9002", 1)
	rr := &RoundRobin{}
	backends := []*Backend{b0, b1, b2}
	want := []*Backend{b0, b1, b2, b0, b1}
	for i, w := range want {
		got, err := rr.Select(backends)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if got != w {
			t.Fatalf("round %d: got %s, want %s", i, got.Addr(), w.Addr())
		}
	}
}

func TestRoundRobinSkipsUnhealthyBackend(t *testing.T) {
	t.Parallel()
	b0, _ := NewBackend("127.0.0.1:9000", 1)
	b1, _ := NewBackend("127.0.0.1:9001", 1)
	b1.healthy.Store(false)
	rr := &RoundRobin{}
	for i := 0; i < 6; i++ {
		got, err := rr.Select([]*Backend{b0, b1})
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if got != b0 {
			t.Fatalf("round %d: expected only healthy backend b0, got %s", i, got.Addr())
		}
	}
}

func TestRoundRobinReturnsErrNoBackendsWhenAllUnhealthy(t *testing.T) {
	t.Parallel()
	b, _ := NewBackend("127.0.0.1:9000", 1)
	b.healthy.Store(false)
	rr := &RoundRobin{}
	_, err := rr.Select([]*Backend{b})
	if !errors.Is(err, ErrNoBackends) {
		t.Fatalf("err = %v, want ErrNoBackends", err)
	}
}

// --- unit tests: LeastConnections ---

func TestLeastConnectionsPicksLowestActive(t *testing.T) {
	t.Parallel()
	b0, _ := NewBackend("127.0.0.1:9000", 1)
	b1, _ := NewBackend("127.0.0.1:9001", 1)
	b0.active.Store(5)
	b1.active.Store(2)
	lc := &LeastConnections{}
	got, err := lc.Select([]*Backend{b0, b1})
	if err != nil {
		t.Fatal(err)
	}
	if got != b1 {
		t.Fatalf("expected b1 (active=2), got %s (active=%d)", got.Addr(), got.Active())
	}
}

func TestLeastConnectionsReturnsErrNoBackendsWhenAllUnhealthy(t *testing.T) {
	t.Parallel()
	b, _ := NewBackend("127.0.0.1:9000", 1)
	b.healthy.Store(false)
	lc := &LeastConnections{}
	_, err := lc.Select([]*Backend{b})
	if !errors.Is(err, ErrNoBackends) {
		t.Fatalf("err = %v, want ErrNoBackends", err)
	}
}

// --- integration test: real TCP relay ---

func newEchoServer(t *testing.T) net.Listener {
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
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln
}

func TestLoadBalancerRelaysDataThroughEchoServer(t *testing.T) {
	t.Parallel()

	echo := newEchoServer(t)

	backend, err := NewBackend(echo.Addr().String(), 1)
	if err != nil {
		t.Fatal(err)
	}

	lb, err := New(&RoundRobin{}, backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer lb.Close()

	conn, err := net.Dial("tcp", lb.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	const msg = "hello"
	if _, err := fmt.Fprint(conn, msg); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("echo = %q, want %q", string(buf), msg)
	}
}

func TestRoundRobinDistributesAcrossBackends(t *testing.T) {
	t.Parallel()

	// Two echo backends; send two connections and check both receive traffic.
	echo0 := newEchoServer(t)
	echo1 := newEchoServer(t)

	b0, _ := NewBackend(echo0.Addr().String(), 1)
	b1, _ := NewBackend(echo1.Addr().String(), 1)

	lb, err := New(&RoundRobin{}, b0, b1)
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer lb.Close()

	addr := lb.Addr().String()
	for _, msg := range []string{"first", "second"} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprint(conn, msg)
		buf := make([]byte, len(msg))
		io.ReadFull(conn, buf)
	}

	if b0.Total()+b1.Total() != 2 {
		t.Fatalf("total routed = %d, want 2", b0.Total()+b1.Total())
	}
	if b0.Total() != 1 || b1.Total() != 1 {
		t.Fatalf("expected one connection per backend; b0=%d b1=%d", b0.Total(), b1.Total())
	}
}

func TestSetHealthyMarkesBackendUnhealthy(t *testing.T) {
	t.Parallel()
	b, _ := NewBackend("127.0.0.1:9000", 1)
	lb, _ := New(&RoundRobin{}, b)
	lb.SetHealthy("127.0.0.1:9000", false)
	if b.Healthy() {
		t.Fatal("expected backend to be marked unhealthy after SetHealthy(false)")
	}
	lb.SetHealthy("127.0.0.1:9000", true)
	if !b.Healthy() {
		t.Fatal("expected backend to be marked healthy after SetHealthy(true)")
	}
}

// ExampleRoundRobin shows how to select a backend from a two-backend pool.
func ExampleRoundRobin() {
	b0, _ := NewBackend("127.0.0.1:9000", 1)
	b1, _ := NewBackend("127.0.0.1:9001", 1)
	rr := &RoundRobin{}
	got, _ := rr.Select([]*Backend{b0, b1})
	fmt.Println(got.Addr())
	// Output: 127.0.0.1:9000
}

// Your turn: add TestLeastConnectionsFallsBackToFirstWhenAllEqual that calls
// Select on two backends both at active=0 and asserts no error is returned.
```

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"example.com/lb"
)

// echoServer starts a loopback TCP server that echoes every byte it receives.
func echoServer() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln, nil
}

func main() {
	// Spin up two echo backends.
	echo0, err := echoServer()
	if err != nil {
		log.Fatal(err)
	}
	defer echo0.Close()

	echo1, err := echoServer()
	if err != nil {
		log.Fatal(err)
	}
	defer echo1.Close()

	fmt.Println("backend 0:", echo0.Addr())
	fmt.Println("backend 1:", echo1.Addr())

	b0, err := lb.NewBackend(echo0.Addr().String(), 1)
	if err != nil {
		log.Fatal(err)
	}
	b1, err := lb.NewBackend(echo1.Addr().String(), 1)
	if err != nil {
		log.Fatal(err)
	}

	balancer, err := lb.New(&lb.RoundRobin{}, b0, b1)
	if err != nil {
		log.Fatal(err)
	}
	if err := balancer.Listen("127.0.0.1:0"); err != nil {
		log.Fatal(err)
	}
	defer balancer.Close()

	fmt.Println("load balancer:", balancer.Addr())

	// Send three messages through the balancer; they round-robin to b0, b1, b0.
	for i, msg := range []string{"alpha", "beta", "gamma"} {
		conn, err := net.DialTimeout("tcp", balancer.Addr().String(), 2*time.Second)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Fprint(conn, msg)
		buf := make([]byte, len(msg))
		io.ReadFull(conn, buf)
		conn.Close()
		fmt.Printf("conn %d: sent=%q echoed=%q b0.total=%d b1.total=%d\n",
			i, msg, string(buf), b0.Total(), b1.Total())
	}
}
```

## Common Mistakes

### Not Closing the Destination After io.Copy Returns

Wrong: two goroutines each call `io.Copy` without closing the destination after the copy finishes. When the client sends EOF, the client-to-backend goroutine returns but the backend-to-client goroutine blocks forever waiting for data that the backend will never send because it is waiting for more input from the client.

What happens: the relay goroutines never exit. The `handleConn` goroutine leaks. Under load, goroutine count grows without bound.

Fix: after each `io.Copy` returns, call `dst.Close()`. This makes the reverse copy's source return an error, which unblocks the reverse goroutine.

### Mutating the Round-Robin Index Without a Lock

Wrong:

```go
type RoundRobin struct{ idx int }

func (r *RoundRobin) Select(backends []*Backend) (*Backend, error) {
	b := backends[r.idx%len(backends)]
	r.idx++
	return b, nil
}
```

What happens: the race detector reports a data race on `r.idx` when two goroutines call `Select` concurrently. The index can be read by one goroutine and incremented by another simultaneously.

Fix: protect `r.idx` with a `sync.Mutex` as shown in Exercise 1.

### Decrementing Active Count in the Wrong Place

Wrong:

```go
backend.active.Add(1)
relay(client, upstream)
backend.active.Add(-1) // never reached if relay panics
```

What happens: if `relay` panics or the goroutine is cancelled, the decrement is skipped and the active count drifts upward. `LeastConnections` keeps routing to a backend it believes is idle when it is actually broken.

Fix: use `defer func() { backend.active.Add(-1) }()` immediately after the increment so the decrement runs on every exit path.

### Blocking the Accept Loop With a Synchronous Dial

Wrong:

```go
func (lb *LoadBalancer) acceptLoop(ln net.Listener) {
	for {
		conn, _ := ln.Accept()
		backend, _ := lb.alg.Select(lb.backends)
		upstream, _ := net.Dial("tcp", backend.addr) // blocks here
		relay(conn, upstream)                         // blocks here too
	}
}
```

What happens: the accept loop serializes all connections. One slow backend dial delays every subsequent connection.

Fix: move `handleConn` (dial + relay) into its own goroutine, so `acceptLoop` only accepts and dispatches.

## Verification

From `~/go-exercises/lb`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. Add `TestLeastConnectionsFallsBackToFirstWhenAllEqual` as described in Exercise 2 before running `go test`.

## Summary

- An L4 load balancer relays raw TCP bytes without protocol awareness, using `net.Listener` and `net.Dial`.
- Round-robin requires a mutex on the index; least-connections reads atomic counters.
- Half-close: close the destination connection after each `io.Copy` returns to unblock the reverse goroutine.
- `atomic.Bool` and `atomic.Int64` carry health state and connection counts safely across goroutines.
- Passive health marking on dial failure is quick to implement; restoring backends requires active checks or explicit `SetHealthy` calls.
- `sync.Once` makes `Close` safe to call multiple times; `sync.WaitGroup` drains all relays before returning.

## What's Next

Next: [Building a Port Scanner](../22-building-a-port-scanner/22-building-a-port-scanner.md).

## Resources

- [net package — pkg.go.dev](https://pkg.go.dev/net) — `Listener`, `Conn`, `Dial`, `Listen`, and their concurrency guarantees
- [sync/atomic package — pkg.go.dev](https://pkg.go.dev/sync/atomic) — `Bool` and `Int64` types added in Go 1.19
- [io.Copy — pkg.go.dev](https://pkg.go.dev/io#Copy) — the primary relay primitive; returns on EOF or error
- [The Go Blog: Share Memory by Communicating](https://go.dev/blog/codelab-share) — when to use channels vs. atomics vs. mutexes
- [HAProxy architecture guide](https://www.haproxy.org/download/2.8/doc/architecture.txt) — authoritative reference on L4/L7 design trade-offs
