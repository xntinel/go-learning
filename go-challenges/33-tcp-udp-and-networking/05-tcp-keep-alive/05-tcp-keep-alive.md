# 5. TCP Keep-Alive

A TCP connection can sit idle indefinitely while both ends hold open file descriptors and memory for a peer that has long since crashed, lost power, or been silently dropped by a NAT gateway. TCP keep-alive sends periodic probe segments on idle connections to detect dead peers before the application layer ever notices. Go 1.23 introduced `net.KeepAliveConfig` for fine-grained control over the three OS-level timing parameters; earlier code called `SetKeepAlive` and `SetKeepAlivePeriod` directly on `*net.TCPConn`. This lesson builds a server that combines OS-level TCP keep-alive with an application-level PING/PONG heartbeat — the combination handles environments where NAT boxes and firewalls silently drop keep-alive probes without sending RST.

```text
keepalive/
  go.mod
  server.go
  server_test.go
  cmd/demo/main.go
```

The package defines a `Config` type, a `Server` that applies keep-alive to every accepted connection via `ListenConfig.KeepAliveConfig`, and a heartbeat handler that closes connections that miss a PING within a configurable window.

## Concepts

### Why Dead Connections Accumulate

When a host crashes, loses network, or sits behind a stateful firewall that silently removes the NAT mapping, no FIN or RST reaches the surviving peer. The server's TCP stack treats the socket as open. The application goroutine blocks on `Read` forever. File descriptors leak. Under load, a server can exhaust the OS fd limit with ghost connections — connections that look healthy but will never produce data.

### The Three OS-Level Parameters

TCP keep-alive is controlled by three timing knobs:

- `Idle` (TCP_KEEPIDLE): how long the connection must be idle before the kernel sends the first probe. A fresh write resets this timer.
- `Interval` (TCP_KEEPINTVL): how long to wait between successive probes once probing starts.
- `Count` (TCP_KEEPCNT): how many unacknowledged probes before the kernel declares the connection dead and closes it.

Total detection time is at most `Idle + Interval * Count`. With `Idle=30s`, `Interval=10s`, `Count=3`, the kernel declares a dead peer within roughly 60 seconds.

OS defaults vary: Linux typically uses `Idle=7200s`, `Interval=75s`, `Count=9` — a two-hour default that is useless for most services. Always set explicit values.

### net.KeepAliveConfig (Go 1.23+)

Go 1.23 added `net.KeepAliveConfig`:

```go
type KeepAliveConfig struct {
	Enable   bool
	Idle     time.Duration
	Interval time.Duration
	Count    int
}
```

Setting `Enable: true` with the other three fields replaces the old `SetKeepAlive` + `SetKeepAlivePeriod` pair and gives access to all three parameters. A zero value for `Idle`, `Interval`, or `Count` means "use the OS default for that parameter".

`net.ListenConfig` gains a `KeepAliveConfig` field: when `Enable` is true, the kernel applies the config automatically to every connection returned by `Accept`. This is the preferred approach — no per-connection call required.

`(*net.TCPConn).SetKeepAliveConfig(config KeepAliveConfig) error` is the per-connection alternative, useful when the listener is not created through `ListenConfig` (for example, when you receive a connection from an `os.File` or accept through a third-party listener).

### Application-Level Heartbeat as a Complement

TCP keep-alive is handled by the kernel, but many corporate firewalls and cloud NAT boxes filter keep-alive probes without sending RST. The application never sees an error; the connection is silently dead. Application-level heartbeats — a client-driven PING with a server-enforced read deadline — detect dead peers even when OS probes are filtered. The two mechanisms are complementary: TCP keep-alive catches dead peers when the application is idle, and the heartbeat catches them during active communication.

## Exercises

This is a library package, not a program: verification is done with `go test`.

### Exercise 1: Config, Server, and the Listener

Create `server.go`:

```go
package keepalive

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// Config holds the OS-level keep-alive parameters and the application-level
// heartbeat timeout for a Server.
type Config struct {
	// Enable turns TCP keep-alive probing on or off for accepted connections.
	Enable bool
	// Idle is how long a connection must be idle before the first probe is sent.
	Idle time.Duration
	// Interval is the time between successive probes once probing has started.
	Interval time.Duration
	// Count is the number of unacknowledged probes before the OS closes the
	// connection. A zero value uses the OS default.
	Count int
	// HeartbeatTimeout is the application-level read deadline per PING.
	// The server closes the connection if no PING arrives within this window.
	HeartbeatTimeout time.Duration
}

// DefaultConfig returns conservative keep-alive and heartbeat defaults suitable
// for most server workloads.
func DefaultConfig() Config {
	return Config{
		Enable:           true,
		Idle:             30 * time.Second,
		Interval:         10 * time.Second,
		Count:            3,
		HeartbeatTimeout: 15 * time.Second,
	}
}

// Server wraps a TCP listener and enforces keep-alive and heartbeat policy on
// every accepted connection.
type Server struct {
	cfg Config
	ln  net.Listener
}

// New creates a Server bound to addr. Use addr "127.0.0.1:0" in tests to let
// the OS pick a free port. The listener is ready immediately; call Serve to
// start accepting connections.
func New(addr string, cfg Config) (*Server, error) {
	lc := net.ListenConfig{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   cfg.Enable,
			Idle:     cfg.Idle,
			Interval: cfg.Interval,
			Count:    cfg.Count,
		},
	}
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("keepalive.New: %w", err)
	}
	return &Server{cfg: cfg, ln: ln}, nil
}

// Addr returns the address the server is listening on, e.g. "127.0.0.1:54321".
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// Close shuts the listener down and causes Serve to return.
func (s *Server) Close() error {
	return s.ln.Close()
}

// Serve accepts connections until ctx is cancelled or Close is called. Each
// connection is handled in its own goroutine.
func (s *Server) Serve(ctx context.Context) error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return fmt.Errorf("keepalive: accept: %w", err)
			}
		}
		go s.handle(conn)
	}
}

// handle speaks the heartbeat protocol: it sets a per-message read deadline and
// responds "PONG\n" to every "PING" line. If the deadline fires (no PING
// received in time), the connection is closed.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for {
		conn.SetReadDeadline(time.Now().Add(s.cfg.HeartbeatTimeout))
		if !sc.Scan() {
			return
		}
		if strings.TrimSpace(sc.Text()) == "PING" {
			fmt.Fprintln(conn, "PONG")
		}
	}
}
```

`ListenConfig.KeepAliveConfig` with `Enable: true` applies the OS-level parameters to every connection returned by `Accept` — no per-connection call is needed. The `handle` goroutine enforces the application-level heartbeat with `SetReadDeadline`: if no PING arrives within `HeartbeatTimeout`, `sc.Scan()` returns false and the connection is closed.

### Exercise 2: Tests and the Heartbeat Verification

Create `server_test.go`:

```go
package keepalive

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// startServer is a test helper that binds to a random port and schedules cleanup.
func startServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	s, err := New("127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)
	t.Cleanup(func() {
		cancel()
		s.Close()
	})
	return s
}

func TestPingPong(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.HeartbeatTimeout = 2 * time.Second
	s := startServer(t, cfg)

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintln(conn, "PING")
	conn.SetReadDeadline(time.Now().Add(time.Second))
	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(resp) != "PONG" {
		t.Fatalf("got %q, want PONG", resp)
	}
}

func TestHeartbeatTimeout(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.HeartbeatTimeout = 100 * time.Millisecond
	s := startServer(t, cfg)

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Send nothing; the server's read deadline fires after 100 ms and closes
	// the server side. The client read should return an error.
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	_, readErr := conn.Read(buf)
	if readErr == nil {
		t.Fatal("expected server to close the connection after heartbeat timeout")
	}
}

func TestMultiplePings(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.HeartbeatTimeout = 2 * time.Second
	s := startServer(t, cfg)

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rd := bufio.NewReader(conn)

	for i := 0; i < 3; i++ {
		fmt.Fprintln(conn, "PING")
		conn.SetReadDeadline(time.Now().Add(time.Second))
		resp, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("ping %d: read: %v", i, err)
		}
		if strings.TrimSpace(resp) != "PONG" {
			t.Fatalf("ping %d: got %q, want PONG", i, resp)
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	if !cfg.Enable {
		t.Error("Enable should be true by default")
	}
	if cfg.Idle <= 0 {
		t.Errorf("Idle = %v, want > 0", cfg.Idle)
	}
	if cfg.Interval <= 0 {
		t.Errorf("Interval = %v, want > 0", cfg.Interval)
	}
	if cfg.Count <= 0 {
		t.Errorf("Count = %d, want > 0", cfg.Count)
	}
	if cfg.HeartbeatTimeout <= 0 {
		t.Errorf("HeartbeatTimeout = %v, want > 0", cfg.HeartbeatTimeout)
	}
}

func ExampleDefaultConfig() {
	cfg := DefaultConfig()
	fmt.Printf("enable=%v idle=%s interval=%s count=%d\n",
		cfg.Enable, cfg.Idle, cfg.Interval, cfg.Count)
	// Output: enable=true idle=30s interval=10s count=3
}
```

Your turn: add `TestConcurrentClients` that starts five goroutines, each dialing the server, sending one PING, and asserting PONG — all concurrently. The test must call `t.Parallel()` and use a `sync.WaitGroup` to collect errors.

### Exercise 3: Command-Line Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"example.com/keepalive"
)

func main() {
	cfg := keepalive.DefaultConfig()
	cfg.HeartbeatTimeout = 3 * time.Second

	srv, err := keepalive.New("127.0.0.1:0", cfg)
	if err != nil {
		fmt.Printf("server error: %v\n", err)
		return
	}
	fmt.Printf("server listening on %s\n", srv.Addr())

	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		fmt.Printf("dial error: %v\n", err)
		cancel()
		srv.Close()
		return
	}

	rd := bufio.NewReader(conn)
	for i := 0; i < 3; i++ {
		fmt.Fprintln(conn, "PING")
		conn.SetReadDeadline(time.Now().Add(time.Second))
		resp, err := rd.ReadString('\n')
		if err != nil {
			fmt.Printf("read error: %v\n", err)
			break
		}
		fmt.Printf("sent PING, got %s", strings.TrimSpace(resp))
		fmt.Println()
		time.Sleep(50 * time.Millisecond)
	}

	conn.Close()
	cancel()
	srv.Close()
}
```

Run with `go run ./cmd/demo`. The demo starts an in-process server, exchanges three PING/PONG pairs, then shuts down.

## Common Mistakes

Wrong: calling `SetKeepAlivePeriod` and expecting it to set `Idle`, `Interval`, and `Count` all at once.

What happens: `SetKeepAlivePeriod(d)` sets only the idle time (and on some platforms the interval too). There is no Go 1.22-or-earlier way to set Count — it stays at the OS default. Use `net.KeepAliveConfig` (Go 1.23+) for all three.

Fix: `tc.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: 30*time.Second, Interval: 10*time.Second, Count: 3})`

---

Wrong: relying on TCP keep-alive alone in cloud and containerized environments.

What happens: many NAT gateways, firewalls, and cloud load balancers silently discard TCP keep-alive probes. The OS never sends an error. The application goroutine blocks on `Read` indefinitely.

Fix: pair TCP keep-alive with a `SetReadDeadline`-enforced heartbeat protocol. The read deadline fires even when TCP probes are filtered.

---

Wrong: setting `HeartbeatTimeout` without resetting it before each read.

What happens: the first `SetReadDeadline` call sets an absolute time. After the first PING/PONG round, the deadline is in the past. Every subsequent `Scan()` returns immediately with a timeout error.

Fix: call `conn.SetReadDeadline(time.Now().Add(s.cfg.HeartbeatTimeout))` inside the loop, before each `Scan()`, as the lesson's `handle` function does.

---

Wrong: creating a new `bufio.Reader` inside a loop over the same connection.

What happens: each new `bufio.Reader` starts with an empty buffer. If the previous reader had buffered data from a syscall, that data is discarded. Later reads miss lines.

Fix: create one `bufio.Reader` (or `bufio.Scanner`) per connection, outside the loop. The lesson's `handle` and `TestMultiplePings` both follow this pattern.

## Verification

From `~/go-exercises/keepalive`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must pass. `go test` is the verification — there is no program to eyeball. The race detector catches the goroutine in `Serve` sharing the listener across `Accept` and `Close`.

## Summary

- TCP keep-alive sends probe segments on idle connections; the three parameters are `Idle`, `Interval`, and `Count`.
- OS defaults (Linux: `Idle=7200s`) are impractical; always set explicit values.
- `net.KeepAliveConfig` (Go 1.23+) gives access to all three parameters via a single struct.
- Setting `ListenConfig.KeepAliveConfig` applies keep-alive to every accepted connection automatically, without a per-connection call.
- Application-level heartbeats (`SetReadDeadline` + PING/PONG) defend against environments where TCP probes are filtered by NAT or firewalls.
- Use both mechanisms together: OS keep-alive for idle connections, heartbeat deadline for active ones.

## What's Next

Next: [Building a Line-Based Protocol](../06-building-a-line-based-protocol/06-building-a-line-based-protocol.md).

## Resources

- [net.KeepAliveConfig — pkg.go.dev](https://pkg.go.dev/net#KeepAliveConfig)
- [net.ListenConfig — pkg.go.dev](https://pkg.go.dev/net#ListenConfig)
- [net.TCPConn.SetKeepAliveConfig — pkg.go.dev](https://pkg.go.dev/net#TCPConn.SetKeepAliveConfig)
- [Go 1.23 Release Notes: net package](https://go.dev/doc/go1.23#net)
- [TCP Keepalive HOWTO — tldp.org](https://tldp.org/HOWTO/TCP-Keepalive-HOWTO/)
