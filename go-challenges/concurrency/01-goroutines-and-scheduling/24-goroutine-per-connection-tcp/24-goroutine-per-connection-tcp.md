# Exercise 24: Goroutine-Per-Connection TCP Server

Every Go HTTP server, gRPC server, and database proxy uses the same model under
the hood: when a connection is accepted, the server spawns one goroutine
dedicated to that connection for its entire lifetime. `net/http` does exactly
this with `go c.serve(connCtx)`. It is the pattern that makes Go's networking so
simple -- thousands of concurrent connections, each with linear control flow, no
callbacks or event loops -- and the failure mode is subtle: without tracking
those goroutines, a shutdown returns while connections are still live and
in-flight requests vanish. This exercise builds that server from scratch: accept
loop, connection registry, limits, broadcast, and a clean four-phase shutdown.

## What you'll build

```text
24-goroutine-per-connection-tcp/
  main.go        TCP echo server, connection registry with limits,
                 broadcast, and graceful drain-and-shutdown
```

- Build: a goroutine-per-connection TCP server with lifecycle management.
- Implement: `TCPServer`/`AcceptLoop`/`handleConnection`; a mutex-protected `ConnectionRegistry` (Register, Unregister, Snapshot) that enforces `maxConnections`; `Broadcast`; and a `GracefulShutdown` that stops accepting, warns, closes, and waits.
- Verify: `go run main.go`

### Why goroutine-per-connection scales

Building a TCP server from scratch teaches you what the standard library hides:
how to accept connections in a loop, how to track active connections so you can
shut down cleanly, how to enforce connection limits so a traffic spike does not
exhaust memory, and how to broadcast messages to all connected clients. These
are not academic exercises -- every production server needs connection tracking
for observability, limits for stability, and clean shutdown for zero-downtime
deploys.

The goroutine-per-connection model works because goroutines are cheap (2-4KB
initial stack) and the scheduler handles blocking I/O efficiently. A Go server
handling 10,000 concurrent connections uses 10,000 goroutines -- each with its
own stack, its own local variables, and linear control flow. No callbacks, no
event loops, no state machines. This simplicity is Go's superpower for network
programming.


## Step 1 -- Basic TCP Server with Connection Goroutines

Build the simplest TCP server: accept connections, spawn a goroutine per connection, echo messages back.

```go
package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	listenAddr     = "127.0.0.1:0"
	numTestClients = 3
)

type TCPServer struct {
	listener net.Listener
	wg       sync.WaitGroup
}

func NewTCPServer(addr string) (*TCPServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	return &TCPServer{listener: ln}, nil
}

func (s *TCPServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *TCPServer) AcceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handleConnection(conn)
	}
}

func (s *TCPServer) handleConnection(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	remote := conn.RemoteAddr().String()
	fmt.Printf("  [server] connected: %s\n", remote)

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		msg := scanner.Text()
		if strings.TrimSpace(msg) == "QUIT" {
			fmt.Fprintf(conn, "BYE\n")
			break
		}
		reply := fmt.Sprintf("ECHO: %s\n", msg)
		fmt.Fprint(conn, reply)
	}

	fmt.Printf("  [server] disconnected: %s\n", remote)
}

func (s *TCPServer) Shutdown() {
	s.listener.Close()
	s.wg.Wait()
}

func runClient(id int, addr string, wg *sync.WaitGroup) {
	defer wg.Done()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Printf("  client %d: connect failed: %v\n", id, err)
		return
	}
	defer conn.Close()

	messages := []string{"hello", "world", "QUIT"}
	reader := bufio.NewReader(conn)

	for _, msg := range messages {
		fmt.Fprintf(conn, "%s\n", msg)
		reply, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		fmt.Printf("  client %d: sent=%q recv=%q\n", id, msg, strings.TrimSpace(reply))
	}
}

func main() {
	server, err := NewTCPServer(listenAddr)
	if err != nil {
		fmt.Printf("Failed to start server: %v\n", err)
		return
	}

	fmt.Printf("=== TCP Echo Server ===\n")
	fmt.Printf("  Listening on %s\n\n", server.Addr())

	go server.AcceptLoop()

	time.Sleep(50 * time.Millisecond)

	var clientWg sync.WaitGroup
	for i := 1; i <= numTestClients; i++ {
		clientWg.Add(1)
		go runClient(i, server.Addr(), &clientWg)
	}

	clientWg.Wait()
	time.Sleep(100 * time.Millisecond)

	server.Shutdown()
	fmt.Println()
	fmt.Println("  Server shut down cleanly")
}
```

**What's happening here:** `AcceptLoop` runs in a goroutine, blocking on `listener.Accept()`. Each accepted connection spawns a new goroutine (`handleConnection`) that reads lines, echoes them back, and exits when the client sends "QUIT" or disconnects. `Shutdown` closes the listener (which causes `Accept` to return an error, breaking the loop), then waits for all connection goroutines to finish via `WaitGroup`.

**Key insight:** Closing the listener is how you stop accepting new connections. Existing connections continue running in their goroutines until they complete naturally. The `WaitGroup` ensures `Shutdown` blocks until every connection goroutine has exited. This is the same pattern Go's `http.Server.Shutdown` uses internally.

### Verification
```bash
go run main.go
```
Expected output (order of client lines varies):
```
=== TCP Echo Server ===
  Listening on 127.0.0.1:52341

  [server] connected: 127.0.0.1:52342
  [server] connected: 127.0.0.1:52343
  [server] connected: 127.0.0.1:52344
  client 1: sent="hello" recv="ECHO: hello"
  client 2: sent="hello" recv="ECHO: hello"
  client 3: sent="hello" recv="ECHO: hello"
  client 1: sent="world" recv="ECHO: world"
  client 2: sent="world" recv="ECHO: world"
  client 3: sent="world" recv="ECHO: world"
  client 1: sent="QUIT" recv="BYE"
  client 2: sent="QUIT" recv="BYE"
  client 3: sent="QUIT" recv="BYE"
  [server] disconnected: 127.0.0.1:52342
  [server] disconnected: 127.0.0.1:52343
  [server] disconnected: 127.0.0.1:52344

  Server shut down cleanly
```


## Step 2 -- Connection Registry and Limits

Add a connection registry that tracks active connections, enforces a maximum connection limit, and provides real-time stats.

```go
package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	serverAddr     = "127.0.0.1:0"
	maxConnections = 3
	totalClients   = 5
)

type ConnectionInfo struct {
	ID        int
	Addr      string
	Connected time.Time
}

type ConnectionRegistry struct {
	mu     sync.Mutex
	conns  map[int]*ConnectionInfo
	nextID int
}

func NewConnectionRegistry() *ConnectionRegistry {
	return &ConnectionRegistry{
		conns: make(map[int]*ConnectionInfo),
	}
}

func (r *ConnectionRegistry) Register(addr string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.conns) >= maxConnections {
		return 0, false
	}

	r.nextID++
	r.conns[r.nextID] = &ConnectionInfo{
		ID:        r.nextID,
		Addr:      addr,
		Connected: time.Now(),
	}
	return r.nextID, true
}

func (r *ConnectionRegistry) Unregister(id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, id)
}

func (r *ConnectionRegistry) ActiveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

func (r *ConnectionRegistry) Snapshot() []ConnectionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]ConnectionInfo, 0, len(r.conns))
	for _, info := range r.conns {
		result = append(result, *info)
	}
	return result
}

type LimitedTCPServer struct {
	listener net.Listener
	registry *ConnectionRegistry
	wg       sync.WaitGroup
	rejected int64
}

func NewLimitedTCPServer(addr string) (*LimitedTCPServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	return &LimitedTCPServer{
		listener: ln,
		registry: NewConnectionRegistry(),
	}, nil
}

func (s *LimitedTCPServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *LimitedTCPServer) AcceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		connID, accepted := s.registry.Register(conn.RemoteAddr().String())
		if !accepted {
			fmt.Fprintf(conn, "ERROR: server full (%d/%d connections)\n", maxConnections, maxConnections)
			conn.Close()
			atomic.AddInt64(&s.rejected, 1)
			fmt.Printf("  [server] REJECTED %s (limit: %d)\n", conn.RemoteAddr(), maxConnections)
			continue
		}

		s.wg.Add(1)
		go s.handleConnection(connID, conn)
	}
}

func (s *LimitedTCPServer) handleConnection(connID int, conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	defer s.registry.Unregister(connID)

	remote := conn.RemoteAddr().String()
	fmt.Printf("  [server] +conn %d from %s (active: %d/%d)\n",
		connID, remote, s.registry.ActiveCount(), maxConnections)

	fmt.Fprintf(conn, "WELCOME conn-%d\n", connID)

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		msg := scanner.Text()
		if strings.TrimSpace(msg) == "QUIT" {
			fmt.Fprintf(conn, "BYE conn-%d\n", connID)
			break
		}
		fmt.Fprintf(conn, "ECHO[%d]: %s\n", connID, msg)
	}

	fmt.Printf("  [server] -conn %d from %s (active: %d/%d)\n",
		connID, remote, s.registry.ActiveCount(), maxConnections)
}

func (s *LimitedTCPServer) Shutdown() {
	s.listener.Close()
	s.wg.Wait()
}

func clientWorker(id int, addr string, delay time.Duration, wg *sync.WaitGroup, results chan<- string) {
	defer wg.Done()

	time.Sleep(delay)

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		results <- fmt.Sprintf("client %d: connect failed: %v", id, err)
		return
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	welcome, _ := reader.ReadString('\n')
	welcome = strings.TrimSpace(welcome)

	if strings.HasPrefix(welcome, "ERROR") {
		results <- fmt.Sprintf("client %d: %s", id, welcome)
		return
	}

	fmt.Fprintf(conn, "ping from client %d\n", id)
	reply, _ := reader.ReadString('\n')

	time.Sleep(300 * time.Millisecond)

	fmt.Fprintf(conn, "QUIT\n")
	bye, _ := reader.ReadString('\n')

	results <- fmt.Sprintf("client %d: welcome=%q reply=%q bye=%q",
		id, welcome, strings.TrimSpace(reply), strings.TrimSpace(bye))
}

func main() {
	server, err := NewLimitedTCPServer(serverAddr)
	if err != nil {
		fmt.Printf("Failed to start: %v\n", err)
		return
	}

	fmt.Printf("=== TCP Server with Connection Limits ===\n")
	fmt.Printf("  Address: %s\n", server.Addr())
	fmt.Printf("  Max connections: %d\n", maxConnections)
	fmt.Printf("  Total clients: %d (expect %d rejected)\n\n", totalClients, totalClients-maxConnections)

	go server.AcceptLoop()
	time.Sleep(50 * time.Millisecond)

	var clientWg sync.WaitGroup
	results := make(chan string, totalClients)

	for i := 1; i <= totalClients; i++ {
		clientWg.Add(1)
		go clientWorker(i, server.Addr(), 0, &clientWg, results)
	}

	clientWg.Wait()
	close(results)

	fmt.Println()
	fmt.Println("--- Client Results ---")
	for r := range results {
		fmt.Printf("  %s\n", r)
	}

	fmt.Printf("\n--- Server Stats ---\n")
	fmt.Printf("  Rejected: %d\n", atomic.LoadInt64(&server.rejected))
	fmt.Printf("  Active: %d\n", server.registry.ActiveCount())

	server.Shutdown()
	fmt.Println("  Server shut down cleanly")
}
```

**What's happening here:** The `ConnectionRegistry` tracks every active connection with an ID, remote address, and connection timestamp. Before spawning a connection goroutine, the accept loop checks if the registry has capacity. If the limit is reached, the connection is immediately rejected with an error message and closed. This prevents resource exhaustion under connection storms.

**Key insight:** The connection limit is checked *before* spawning the goroutine, not after. If you spawned the goroutine first and then checked, you would briefly exceed the limit -- and under a SYN flood, "briefly" means thousands of goroutines. The registry is mutex-protected because `Accept`, `handleConnection`, and the stats dashboard all access it from different goroutines.

### Verification
```bash
go run main.go
```
Expected output (order varies, 2 clients should be rejected):
```
=== TCP Server with Connection Limits ===
  Address: 127.0.0.1:52341
  Max connections: 3
  Total clients: 5 (expect 2 rejected)

  [server] +conn 1 from 127.0.0.1:52342 (active: 1/3)
  [server] +conn 2 from 127.0.0.1:52343 (active: 2/3)
  [server] +conn 3 from 127.0.0.1:52344 (active: 3/3)
  [server] REJECTED 127.0.0.1:52345 (limit: 3)
  [server] REJECTED 127.0.0.1:52346 (limit: 3)
  [server] -conn 1 from 127.0.0.1:52342 (active: 2/3)
  [server] -conn 2 from 127.0.0.1:52343 (active: 1/3)
  [server] -conn 3 from 127.0.0.1:52344 (active: 0/3)

--- Client Results ---
  client 4: ERROR: server full (3/3 connections)
  client 5: ERROR: server full (3/3 connections)
  client 1: welcome="WELCOME conn-1" reply="ECHO[1]: ping from client 1" bye="BYE conn-1"
  client 2: welcome="WELCOME conn-2" reply="ECHO[2]: ping from client 2" bye="BYE conn-2"
  client 3: welcome="WELCOME conn-3" reply="ECHO[3]: ping from client 3" bye="BYE conn-3"

--- Server Stats ---
  Rejected: 2
  Active: 0
  Server shut down cleanly
```


## Step 3 -- Broadcast and Dashboard

Add a broadcast mechanism that sends a message to all connected clients, and a dashboard goroutine that prints real-time connection stats.

```go
package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	srvAddr         = "127.0.0.1:0"
	srvMaxConns     = 10
	srvClients      = 4
	dashboardTick   = 500 * time.Millisecond
)

type ConnEntry struct {
	ID     int
	Addr   string
	Conn   net.Conn
	Joined time.Time
}

type ConnTracker struct {
	mu     sync.Mutex
	conns  map[int]*ConnEntry
	nextID int
}

func NewConnTracker() *ConnTracker {
	return &ConnTracker{conns: make(map[int]*ConnEntry)}
}

func (t *ConnTracker) Add(conn net.Conn) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.conns) >= srvMaxConns {
		return 0, false
	}

	t.nextID++
	t.conns[t.nextID] = &ConnEntry{
		ID:     t.nextID,
		Addr:   conn.RemoteAddr().String(),
		Conn:   conn,
		Joined: time.Now(),
	}
	return t.nextID, true
}

func (t *ConnTracker) Remove(id int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conns, id)
}

func (t *ConnTracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

func (t *ConnTracker) Broadcast(msg string) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	sent := 0
	for _, entry := range t.conns {
		_, err := fmt.Fprintf(entry.Conn, "BROADCAST: %s\n", msg)
		if err == nil {
			sent++
		}
	}
	return sent
}

type BroadcastServer struct {
	listener  net.Listener
	tracker   *ConnTracker
	wg        sync.WaitGroup
	dashStop  chan struct{}
	messages  int64
	rejected  int64
}

func NewBroadcastServer(addr string) (*BroadcastServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	return &BroadcastServer{
		listener: ln,
		tracker:  NewConnTracker(),
		dashStop: make(chan struct{}),
	}, nil
}

func (s *BroadcastServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *BroadcastServer) AcceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		connID, ok := s.tracker.Add(conn)
		if !ok {
			fmt.Fprintf(conn, "ERROR: server full\n")
			conn.Close()
			atomic.AddInt64(&s.rejected, 1)
			continue
		}

		s.wg.Add(1)
		go s.handleConn(connID, conn)
	}
}

func (s *BroadcastServer) handleConn(id int, conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	defer s.tracker.Remove(id)

	fmt.Fprintf(conn, "WELCOME conn-%d\n", id)

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		msg := strings.TrimSpace(scanner.Text())
		atomic.AddInt64(&s.messages, 1)

		if msg == "QUIT" {
			fmt.Fprintf(conn, "BYE\n")
			return
		}

		fmt.Fprintf(conn, "ECHO[%d]: %s\n", id, msg)
	}
}

func (s *BroadcastServer) StartDashboard() {
	go func() {
		ticker := time.NewTicker(dashboardTick)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				fmt.Printf("  [dashboard] conns=%d msgs=%d rejected=%d\n",
					s.tracker.Count(),
					atomic.LoadInt64(&s.messages),
					atomic.LoadInt64(&s.rejected))
			case <-s.dashStop:
				return
			}
		}
	}()
}

func (s *BroadcastServer) Broadcast(msg string) int {
	return s.tracker.Broadcast(msg)
}

func (s *BroadcastServer) Shutdown() {
	close(s.dashStop)
	s.listener.Close()
	s.wg.Wait()
}

func chatClient(id int, addr string, ready chan<- struct{}, done chan<- string) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		done <- fmt.Sprintf("client %d: connect failed: %v", id, err)
		return
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	welcome, _ := reader.ReadString('\n')
	_ = welcome

	ready <- struct{}{}

	var received []string
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		received = append(received, line)

		if strings.HasPrefix(line, "BYE") || len(received) >= 3 {
			break
		}
	}

	done <- fmt.Sprintf("client %d: received %d messages: %v", id, len(received), received)
}

func main() {
	server, err := NewBroadcastServer(srvAddr)
	if err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}

	fmt.Printf("=== TCP Server with Broadcast ===\n")
	fmt.Printf("  Address: %s\n\n", server.Addr())

	go server.AcceptLoop()
	server.StartDashboard()
	time.Sleep(50 * time.Millisecond)

	readyCh := make(chan struct{}, srvClients)
	doneCh := make(chan string, srvClients)

	for i := 1; i <= srvClients; i++ {
		go chatClient(i, server.Addr(), readyCh, doneCh)
	}

	for i := 0; i < srvClients; i++ {
		<-readyCh
	}
	fmt.Printf("  All %d clients connected\n\n", srvClients)

	time.Sleep(200 * time.Millisecond)

	fmt.Println("  Broadcasting 'maintenance in 5 minutes'...")
	sent := server.Broadcast("maintenance in 5 minutes")
	fmt.Printf("  Broadcast sent to %d clients\n\n", sent)

	time.Sleep(200 * time.Millisecond)

	fmt.Println("  Broadcasting 'shutdown imminent'...")
	sent = server.Broadcast("shutdown imminent")
	fmt.Printf("  Broadcast sent to %d clients\n", sent)

	time.Sleep(1 * time.Second)

	fmt.Println()
	fmt.Println("--- Client Results ---")
	for i := 0; i < srvClients; i++ {
		select {
		case r := <-doneCh:
			fmt.Printf("  %s\n", r)
		case <-time.After(2 * time.Second):
			fmt.Println("  (client timed out)")
		}
	}

	server.Shutdown()
	fmt.Println()
	fmt.Println("  Server shut down cleanly")
}
```

**What's happening here:** `ConnTracker.Broadcast` iterates over all registered connections and writes to each one. The mutex protects the iteration -- without it, a connection closing during broadcast could cause a write to a closed connection. The dashboard goroutine prints stats on a ticker. Clients signal readiness via a channel so the server knows when all are connected before broadcasting.

**Key insight:** Broadcasting requires holding the tracker mutex while writing to connections. If a connection's write buffer is full, `Fprintf` blocks, holding the mutex and blocking all other operations (new connections, disconnections, other broadcasts). In production, you would use a per-connection write channel to decouple broadcast from the registry lock. For this exercise, the direct approach is simpler and demonstrates the tradeoff.

### Verification
```bash
go run main.go
```
Expected output:
```
=== TCP Server with Broadcast ===
  Address: 127.0.0.1:52341

  All 4 clients connected

  [dashboard] conns=4 msgs=0 rejected=0
  Broadcasting 'maintenance in 5 minutes'...
  Broadcast sent to 4 clients

  Broadcasting 'shutdown imminent'...
  Broadcast sent to 4 clients
  [dashboard] conns=4 msgs=0 rejected=0

--- Client Results ---
  client 1: received 2 messages: [BROADCAST: maintenance in 5 minutes BROADCAST: shutdown imminent]
  client 2: received 2 messages: [BROADCAST: maintenance in 5 minutes BROADCAST: shutdown imminent]
  client 3: received 2 messages: [BROADCAST: maintenance in 5 minutes BROADCAST: shutdown imminent]
  client 4: received 2 messages: [BROADCAST: maintenance in 5 minutes BROADCAST: shutdown imminent]

  Server shut down cleanly
```


## Step 4 -- Full Server with Clean Shutdown

Combine everything into a production-grade server with connection limits, broadcast, dashboard, and graceful shutdown that drains active connections.

```go
package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	fullAddr       = "127.0.0.1:0"
	fullMaxConns   = 5
	fullClients    = 6
	fullDashTick   = 400 * time.Millisecond
	shutdownNotice = 500 * time.Millisecond
)

type TrackedConn struct {
	ID     int
	Remote string
	Conn   net.Conn
	Since  time.Time
}

type Registry struct {
	mu      sync.Mutex
	entries map[int]*TrackedConn
	nextID  int
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[int]*TrackedConn)}
}

func (r *Registry) Add(conn net.Conn) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) >= fullMaxConns {
		return 0, false
	}
	r.nextID++
	r.entries[r.nextID] = &TrackedConn{
		ID: r.nextID, Remote: conn.RemoteAddr().String(),
		Conn: conn, Since: time.Now(),
	}
	return r.nextID, true
}

func (r *Registry) Remove(id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}

func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *Registry) BroadcastAndClose(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.entries {
		fmt.Fprintf(entry.Conn, "SERVER: %s\n", msg)
		entry.Conn.Close()
	}
}

func (r *Registry) Broadcast(msg string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	sent := 0
	for _, e := range r.entries {
		if _, err := fmt.Fprintf(e.Conn, "SERVER: %s\n", msg); err == nil {
			sent++
		}
	}
	return sent
}

type FullTCPServer struct {
	listener net.Listener
	registry *Registry
	wg       sync.WaitGroup
	dashStop chan struct{}
	stats    struct {
		totalConns  int64
		totalMsgs   int64
		rejected    int64
	}
}

func NewFullTCPServer(addr string) (*FullTCPServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	return &FullTCPServer{
		listener: ln,
		registry: NewRegistry(),
		dashStop: make(chan struct{}),
	}, nil
}

func (s *FullTCPServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *FullTCPServer) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		atomic.AddInt64(&s.stats.totalConns, 1)

		connID, ok := s.registry.Add(conn)
		if !ok {
			fmt.Fprintf(conn, "ERROR: max connections reached (%d)\n", fullMaxConns)
			conn.Close()
			atomic.AddInt64(&s.stats.rejected, 1)
			continue
		}

		s.wg.Add(1)
		go s.handle(connID, conn)
	}
}

func (s *FullTCPServer) handle(id int, conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	defer s.registry.Remove(id)

	fmt.Fprintf(conn, "CONNECTED as conn-%d (%d/%d slots)\n", id, s.registry.Count(), fullMaxConns)

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		msg := strings.TrimSpace(scanner.Text())
		if msg == "" {
			continue
		}
		atomic.AddInt64(&s.stats.totalMsgs, 1)

		switch {
		case msg == "QUIT":
			fmt.Fprintf(conn, "BYE conn-%d\n", id)
			return
		case msg == "STATS":
			fmt.Fprintf(conn, "STATS: conns=%d total=%d msgs=%d rejected=%d\n",
				s.registry.Count(),
				atomic.LoadInt64(&s.stats.totalConns),
				atomic.LoadInt64(&s.stats.totalMsgs),
				atomic.LoadInt64(&s.stats.rejected))
		default:
			fmt.Fprintf(conn, "[conn-%d] %s\n", id, msg)
		}
	}
}

func (s *FullTCPServer) StartDashboard() {
	go func() {
		ticker := time.NewTicker(fullDashTick)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fmt.Printf("  [dash] active=%d total=%d msgs=%d rejected=%d\n",
					s.registry.Count(),
					atomic.LoadInt64(&s.stats.totalConns),
					atomic.LoadInt64(&s.stats.totalMsgs),
					atomic.LoadInt64(&s.stats.rejected))
			case <-s.dashStop:
				return
			}
		}
	}()
}

func (s *FullTCPServer) GracefulShutdown() {
	fmt.Println("  [shutdown] Phase 1: stop accepting new connections")
	s.listener.Close()

	fmt.Println("  [shutdown] Phase 2: notify connected clients")
	s.registry.Broadcast("server shutting down in 500ms")
	time.Sleep(shutdownNotice)

	fmt.Println("  [shutdown] Phase 3: close all connections")
	s.registry.BroadcastAndClose("goodbye")

	fmt.Println("  [shutdown] Phase 4: wait for goroutines to exit")
	s.wg.Wait()
	close(s.dashStop)

	fmt.Printf("  [shutdown] Complete. Total connections: %d, Messages: %d, Rejected: %d\n",
		atomic.LoadInt64(&s.stats.totalConns),
		atomic.LoadInt64(&s.stats.totalMsgs),
		atomic.LoadInt64(&s.stats.rejected))
}

func fullClient(id int, addr string, connected chan<- int, results chan<- string) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		results <- fmt.Sprintf("client %d: failed to connect: %v", id, err)
		connected <- id
		return
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	welcome, _ := reader.ReadString('\n')
	welcome = strings.TrimSpace(welcome)

	if strings.HasPrefix(welcome, "ERROR") {
		results <- fmt.Sprintf("client %d: rejected: %s", id, welcome)
		connected <- id
		return
	}

	connected <- id

	fmt.Fprintf(conn, "hello from client %d\n", id)
	reply, _ := reader.ReadString('\n')

	fmt.Fprintf(conn, "STATS\n")
	stats, _ := reader.ReadString('\n')

	var serverMsgs []string
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		serverMsgs = append(serverMsgs, line)
		if strings.Contains(line, "goodbye") {
			break
		}
	}

	results <- fmt.Sprintf("client %d: welcome=%q echo=%q stats=%q server_msgs=%v",
		id, welcome, strings.TrimSpace(reply), strings.TrimSpace(stats), serverMsgs)
}

func main() {
	server, err := NewFullTCPServer(fullAddr)
	if err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}

	fmt.Printf("=== Full TCP Server ===\n")
	fmt.Printf("  Address: %s | Max: %d | Clients: %d\n\n", server.Addr(), fullMaxConns, fullClients)

	go server.Serve()
	server.StartDashboard()
	time.Sleep(50 * time.Millisecond)

	connected := make(chan int, fullClients)
	results := make(chan string, fullClients)

	for i := 1; i <= fullClients; i++ {
		go fullClient(i, server.Addr(), connected, results)
	}

	for i := 0; i < fullClients; i++ {
		<-connected
	}

	fmt.Println("  All clients attempted connection")
	fmt.Printf("  Active: %d, Rejected: %d\n\n", server.registry.Count(), atomic.LoadInt64(&server.stats.rejected))

	time.Sleep(1 * time.Second)

	fmt.Println()
	fmt.Println("--- Graceful Shutdown ---")
	server.GracefulShutdown()

	fmt.Println()
	fmt.Println("--- Client Results ---")
	for i := 0; i < fullClients; i++ {
		select {
		case r := <-results:
			fmt.Printf("  %s\n", r)
		case <-time.After(2 * time.Second):
			fmt.Println("  (timeout)")
		}
	}
}
```

**What's happening here:** `GracefulShutdown` executes four phases: (1) close the listener to reject new connections, (2) broadcast a warning to active clients, (3) close all connections after a grace period, (4) wait for all connection goroutines to exit. The 6th client is rejected because the limit is 5. Accepted clients receive echo, stats, and then shutdown notifications.

**Key insight:** Graceful shutdown is essential for zero-downtime deploys. The four-phase approach (stop accepting, warn, close, wait) gives clients time to finish in-flight requests and reconnect to another instance. `BroadcastAndClose` closes connections from the server side, which causes each connection goroutine's `scanner.Scan()` to return false, triggering `defer conn.Close()` and `defer registry.Remove(id)`. Everything cleans up through the normal defer chain.

### Verification
```bash
go run main.go
```
Expected output (order varies):
```
=== Full TCP Server ===
  Address: 127.0.0.1:52341 | Max: 5 | Clients: 6

  All clients attempted connection
  Active: 5, Rejected: 1

  [dash] active=5 total=6 msgs=5 rejected=1
  [dash] active=5 total=6 msgs=10 rejected=1

--- Graceful Shutdown ---
  [shutdown] Phase 1: stop accepting new connections
  [shutdown] Phase 2: notify connected clients
  [shutdown] Phase 3: close all connections
  [shutdown] Phase 4: wait for goroutines to exit
  [shutdown] Complete. Total connections: 6, Messages: 10, Rejected: 1

--- Client Results ---
  client 6: rejected: ERROR: max connections reached (5)
  client 1: welcome="CONNECTED as conn-1 (1/5 slots)" echo="[conn-1] hello from client 1" stats="STATS: conns=5 total=6 msgs=2 rejected=1" server_msgs=[SERVER: server shutting down in 500ms SERVER: goodbye]
  client 2: welcome="CONNECTED as conn-2 (2/5 slots)" echo="[conn-2] hello from client 2" stats="STATS: conns=5 total=6 msgs=4 rejected=1" server_msgs=[SERVER: server shutting down in 500ms SERVER: goodbye]
  client 3: welcome="CONNECTED as conn-3 (3/5 slots)" echo="[conn-3] hello from client 3" stats="STATS: conns=5 total=6 msgs=6 rejected=1" server_msgs=[SERVER: server shutting down in 500ms SERVER: goodbye]
  client 4: welcome="CONNECTED as conn-4 (4/5 slots)" echo="[conn-4] hello from client 4" stats="STATS: conns=5 total=6 msgs=8 rejected=1" server_msgs=[SERVER: server shutting down in 500ms SERVER: goodbye]
  client 5: welcome="CONNECTED as conn-5 (5/5 slots)" echo="[conn-5] hello from client 5" stats="STATS: conns=5 total=6 msgs=10 rejected=1" server_msgs=[SERVER: server shutting down in 500ms SERVER: goodbye]
```


## Common Mistakes

### Not Tracking Connections (Leaking Goroutines on Shutdown)

```go
package main

import (
	"fmt"
	"net"
	"time"
)

func main() {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 1024)
				for {
					_, err := conn.Read(buf) // blocks until client sends or disconnects
					if err != nil {
						return
					}
				}
			}()
			// no WaitGroup, no registry -- these goroutines are invisible
		}
	}()

	time.Sleep(100 * time.Millisecond)
	ln.Close() // stops accepting, but existing connection goroutines are orphaned
	fmt.Println("listener closed, but connection goroutines may still be running")
}
```
**What happens:** Closing the listener stops `Accept`, but connection goroutines are still alive, blocked on `Read`. Without a `WaitGroup` or registry, there is no way to know when they exit. In production, this means your shutdown handler returns, the process exits, and active requests are silently dropped.

**Fix:** Track every connection goroutine with a `WaitGroup`, and close connections explicitly during shutdown to unblock `Read`.


### Writing to a Closed Connection During Broadcast

```go
package main

import (
	"fmt"
	"net"
)

func main() {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	conns := make([]net.Conn, 0)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, conn) // no mutex: data race
		}
	}()

	// Later, broadcast to all connections
	for _, conn := range conns { // data race: iterating while accept loop appends
		fmt.Fprintf(conn, "hello\n") // may write to already-closed connection
	}

	ln.Close()
}
```
**What happens:** Two problems: (1) `conns` is accessed from two goroutines without synchronization, (2) a connection may have closed between being added to the slice and the broadcast. Both cause undefined behavior.

**Fix:** Use a mutex-protected registry (as built in this exercise), and handle write errors gracefully during broadcast.


## Review

The whole server rests on one spawn: every `Accept()` gets its own goroutine, so
each connection runs as ordinary linear code with no callbacks and no shared
state until you introduce it. The registry is what makes that fleet manageable
-- it tracks active connections behind a mutex so you can report stats, enforce
limits, and shut down cleanly. Two ordering rules carry the exercise. The
connection limit is checked *before* spawning the goroutine, because checking
after would briefly overshoot, and under a connection storm "briefly" is
thousands of goroutines. And shutdown runs in four phases -- stop accepting,
warn clients, close connections, wait for goroutines -- where closing each
`net.Conn` from the server side unblocks its goroutine's `Read`, which returns
false from `scanner.Scan()` and drains every deferred `Close`/`Unregister`
through the normal defer chain. This is precisely how `net/http` and its
`Server.Shutdown` behave internally.

To prove you own the pattern, extend it into a chat room without re-reading the
steps. Have clients send a username first; broadcast every later message to all
*other* clients prefixed with `[username]:`; enforce a five-connection limit with
a rejection message; add a `USERS` command; and give it a graceful shutdown that
warns, waits, closes, and waits for goroutines. The one real design constraint
is the one this exercise deliberately left simple: the broadcast must not block
the sender, so decouple it with per-connection buffered write channels instead
of writing directly under the registry lock.

## Resources
- [net.Listener](https://pkg.go.dev/net#Listener) -- the Accept/Close interface the accept loop is built on.
- [net.Conn](https://pkg.go.dev/net#Conn) -- the connection interface (Read, Write, Close) each goroutine drives.
- [net package](https://pkg.go.dev/net) -- Go networking fundamentals for TCP listeners and dialers.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- foundational concurrency patterns for composing goroutines and shutdown signals.
- [http.Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) -- the standard library's production graceful-shutdown, the same four-phase idea.

---

Back to [Concurrency](../../concurrency.md) | Next: [25-goroutine-coordination-barrier](../25-goroutine-coordination-barrier/25-goroutine-coordination-barrier.md)
