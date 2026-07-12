# 3. Concurrent TCP Server

A single-threaded TCP server handles one connection at a time. The moment a second client connects while the first is still active, it waits or is refused. Go's goroutine model changes this: goroutines are cheap (a few KB of stack, cooperatively scheduled), so each accepted connection gets its own goroutine at almost no cost. The real design challenges are resource bounds (file descriptors and memory are finite) and clean shutdown (stopping new connections without killing active ones mid-request).

This lesson builds a reusable `tcpserver` package with a configurable handler, a semaphore-based connection limit, and graceful shutdown that waits for all active handlers to finish.

```text
tcpserver/
  go.mod
  server.go
  server_test.go
  cmd/demo/main.go
```

## Concepts

### The Goroutine-Per-Connection Model

Each call to `ln.Accept()` returns a `net.Conn`. The idiomatic pattern hands it to a new goroutine immediately:

```go
conn, err := ln.Accept()
if err != nil { /* ... */ }
go handleConn(conn)
```

Goroutines are preempted at function calls and channel operations, so thousands coexist on a few OS threads. The main cost is stack memory: each goroutine starts at 2-8 KB and grows as needed. At 100,000 connections that is 200-800 MB just for stacks, which is why connection limits are necessary in production.

### Connection Limiting with a Buffered Channel

A buffered channel of capacity N acts as a counting semaphore. Before spawning a goroutine, the accept loop sends an empty struct into the channel; when the handler finishes, it receives from the channel to release the slot. If the channel is full, the send blocks and the accept loop pauses until a slot opens.

```go
sem := make(chan struct{}, maxConns)
// in the accept loop:
sem <- struct{}{} // acquire — blocks when at capacity
go func() {
	defer func() { <-sem }() // release
	handle(conn)
}()
```

The acquire must happen in the accept loop before `go`, not inside the goroutine. If it happened inside the goroutine, the goroutine would launch immediately and the capacity limit would not be enforced during the burst.

### Graceful Shutdown

A naively written server calls `ln.Close()` on shutdown and nothing else. Active handlers keep running, but if `main` returns the process exits and those handlers are killed mid-request. The correct sequence:

1. Signal the serve loop: close a `quit` channel.
2. Close the listener so `Accept` unblocks and returns an error.
3. Distinguish the shutdown error from real errors by inspecting `<-quit`.
4. Track goroutines with `sync.WaitGroup`; call `wg.Wait()` before returning.

```go
func (s *Server) Shutdown() {
	close(s.quit)
	s.listener.Close()
	s.wg.Wait()
}
```

`sync.Once` prevents a second `Shutdown` call from closing an already-closed channel (which would panic).

### Accept Error Disambiguation

Closing the listener during shutdown makes `Accept` return an error. That error must not be logged as a problem. The select idiom handles both cases:

```go
conn, err := s.listener.Accept()
if err != nil {
	select {
	case <-s.quit:
		return // expected shutdown path
	default:
		continue // transient OS error; retry
	}
}
```

## Exercises

### Exercise 1: The Server Core

Create `server.go`:

```go
package tcpserver

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
)

// ErrMaxConnsZero is returned by WithMaxConns when n is less than 1.
var ErrMaxConnsZero = errors.New("tcpserver: max connections must be at least 1")

// Handler processes a single accepted connection.
// The handler is responsible for closing conn before returning.
type Handler func(conn net.Conn)

// UpperEchoHandler reads newline-delimited lines and writes each back uppercased.
// It closes conn when the remote end closes or an error occurs.
func UpperEchoHandler(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		fmt.Fprintln(conn, strings.ToUpper(sc.Text()))
	}
}

// Server is a concurrent TCP server with a configurable handler,
// a semaphore-based connection limit, and graceful shutdown.
type Server struct {
	listener net.Listener
	handler  Handler
	sem      chan struct{}
	wg       sync.WaitGroup
	quit     chan struct{}
	once     sync.Once
}

// Option configures a Server before it starts accepting.
type Option func(*Server) error

// WithMaxConns limits the number of concurrently active connections.
// The default is 100.
func WithMaxConns(n int) Option {
	return func(s *Server) error {
		if n < 1 {
			return fmt.Errorf("%w: got %d", ErrMaxConnsZero, n)
		}
		s.sem = make(chan struct{}, n)
		return nil
	}
}

// WithHandler replaces the default UpperEchoHandler with h.
func WithHandler(h Handler) Option {
	return func(s *Server) error {
		s.handler = h
		return nil
	}
}

// New creates a Server that listens on addr using TCP.
// Defaults: handler = UpperEchoHandler, maxConns = 100.
func New(addr string, opts ...Option) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcpserver: listen: %w", err)
	}
	s := &Server{
		listener: ln,
		handler:  UpperEchoHandler,
		quit:     make(chan struct{}),
	}
	for _, o := range opts {
		if err := o(s); err != nil {
			ln.Close()
			return nil, err
		}
	}
	if s.sem == nil {
		s.sem = make(chan struct{}, 100)
	}
	return s, nil
}

// Addr returns the network address the server is listening on.
// The value is stable between New and Shutdown.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Serve accepts connections and dispatches each to a goroutine.
// It blocks until Shutdown is called.
func (s *Server) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
				continue
			}
		}
		s.sem <- struct{}{} // acquire a connection slot; blocks at maxConns
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.sem }() // release slot when handler returns
			s.handler(conn)
		}()
	}
}

// Shutdown stops accepting new connections and waits for all active
// connections to finish. It is safe to call Shutdown more than once.
func (s *Server) Shutdown() {
	s.once.Do(func() {
		close(s.quit)
		s.listener.Close()
	})
	s.wg.Wait()
}
```

The semaphore acquire (`s.sem <- struct{}{}`) happens in the accept loop before `go`. The loop blocks when all slots are occupied, which prevents unbounded goroutine growth.

### Exercise 2: Tests

Create `server_test.go`:

```go
package tcpserver

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestWithMaxConnsRejectsZero(t *testing.T) {
	t.Parallel()
	_, err := New(":0", WithMaxConns(0))
	if !errors.Is(err, ErrMaxConnsZero) {
		t.Fatalf("err = %v, want ErrMaxConnsZero", err)
	}
}

func TestWithMaxConnsRejectsNegative(t *testing.T) {
	t.Parallel()
	_, err := New(":0", WithMaxConns(-1))
	if !errors.Is(err, ErrMaxConnsZero) {
		t.Fatalf("err = %v, want ErrMaxConnsZero", err)
	}
}

func TestUpperEchoHandlerEchosUppercase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  string
	}{
		{"hello", "HELLO"},
		{"World", "WORLD"},
		{"go1.26", "GO1.26"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			client, server := net.Pipe()
			defer client.Close()

			go UpperEchoHandler(server)

			fmt.Fprintln(client, tc.input)
			buf := make([]byte, 64)
			n, err := client.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.TrimSpace(string(buf[:n]))
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServeHandlesRequest(t *testing.T) {
	t.Parallel()
	srv, err := New(":0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	conn, err := net.DialTimeout("tcp", srv.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintln(conn, "world")
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(buf[:n]))
	if got != "WORLD" {
		t.Fatalf("got %q, want %q", got, "WORLD")
	}
}

func TestShutdownWaitsForActiveConnections(t *testing.T) {
	t.Parallel()
	slow := make(chan struct{})
	started := make(chan struct{})
	done := make(chan struct{})

	h := func(conn net.Conn) {
		defer conn.Close()
		close(started) // signal that the handler is running
		<-slow         // block until the test releases
	}

	srv, err := New(":0", WithHandler(h))
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()

	conn, err := net.DialTimeout("tcp", srv.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	<-started // wait for the handler goroutine to enter h

	go func() {
		srv.Shutdown()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Shutdown returned before active connection finished")
	case <-time.After(50 * time.Millisecond):
		// good: Shutdown is still waiting for the slow handler
	}

	close(slow) // release the handler

	select {
	case <-done:
		// good: Shutdown returned after the handler finished
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after connection finished")
	}
}

func ExampleUpperEchoHandler() {
	client, server := net.Pipe()
	go UpperEchoHandler(server)
	fmt.Fprintln(client, "hello")
	buf := make([]byte, 8)
	n, _ := client.Read(buf)
	client.Close()
	fmt.Print(string(buf[:n]))
	// Output:
	// HELLO
}
```

Your turn: add `TestAddrReturnsListeningAddress` — call `New(":0")`, verify that `Addr()` is non-empty, and confirm it contains a colon (TCP addresses use the format `host:port`).

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os/signal"
	"syscall"
	"time"

	"example.com/tcpserver"
)

func main() {
	srv, err := tcpserver.New(":0", tcpserver.WithMaxConns(10))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("listening on %s\n", srv.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go srv.Serve()

	// connect once, echo a message, then trigger graceful shutdown
	go func() {
		time.Sleep(50 * time.Millisecond)
		conn, err := net.Dial("tcp", srv.Addr())
		if err != nil {
			log.Printf("dial: %v", err)
			stop()
			return
		}
		defer conn.Close()
		fmt.Fprintln(conn, "hello from demo")
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		fmt.Printf("response: %s", string(buf[:n]))
		stop()
	}()

	<-ctx.Done()
	fmt.Println("shutting down...")
	srv.Shutdown()
	fmt.Println("done")
}
```

Run with `go run ./cmd/demo`.

## Common Mistakes

### Acquiring the Semaphore Inside the Goroutine

Wrong:

```go
go func() {
	sem <- struct{}{} // acquire inside — too late
	defer func() { <-sem }()
	handle(conn)
}()
```

What happens: the goroutine launches immediately, so the accept loop does not block. A burst of 200 connections spawns 200 goroutines before any semaphore slot is acquired; the limit is never enforced.

Fix: acquire before `go`:

```go
sem <- struct{}{} // blocks the accept loop at the limit
s.wg.Add(1)
go func() {
	defer s.wg.Done()
	defer func() { <-sem }()
	handle(conn)
}()
```

### Not Protecting Shutdown with sync.Once

Wrong:

```go
func (s *Server) Shutdown() {
	close(s.quit) // panics on the second call
	s.listener.Close()
	s.wg.Wait()
}
```

What happens: calling `Shutdown` a second time — or from two goroutines simultaneously — closes an already-closed channel, which panics at runtime.

Fix: wrap the close operations in `sync.Once`:

```go
s.once.Do(func() {
	close(s.quit)
	s.listener.Close()
})
s.wg.Wait()
```

### Calling log.Fatal on the Accept Error at Shutdown

Wrong:

```go
conn, err := s.listener.Accept()
if err != nil {
	log.Fatal(err) // kills the process on normal shutdown
}
```

What happens: closing the listener during shutdown causes `Accept` to return an error. `log.Fatal` calls `os.Exit`, which terminates active goroutines immediately instead of letting them finish.

Fix: inspect the `quit` channel to distinguish shutdown from real errors, as shown in the Concepts section.

## Verification

From `~/go-exercises/tcpserver`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must produce no output or error. The `-race` flag catches data races that arise when goroutines share `wg`, `sem`, and `quit` fields during concurrent accept and shutdown.

## Summary

- The goroutine-per-connection model assigns each `net.Conn` to a new goroutine; goroutines are cheap but not free, so a working connection limit is mandatory in production.
- A buffered channel of capacity N acts as a counting semaphore; the acquire must happen in the accept loop, before `go`, not inside the goroutine.
- Graceful shutdown: signal with `close(quit)`, unblock `Accept` with `listener.Close()`, wait with `wg.Wait()`.
- Distinguish the expected shutdown `Accept` error from real errors with `select { case <-s.quit: ... default: ... }`.
- Protect double-shutdown with `sync.Once` to prevent a panic on a closed channel.

## What's Next

Next: [Connection Timeouts and Deadlines](../04-connection-timeouts-and-deadlines/04-connection-timeouts-and-deadlines.md).

## Resources

- [net package](https://pkg.go.dev/net) - Listener, Conn, Dial, Listen
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) - counting active goroutines
- [sync.Once](https://pkg.go.dev/sync#Once) - safe one-time execution
- [os/signal.NotifyContext](https://pkg.go.dev/os/signal#NotifyContext) - context-based signal handling
- [net.Pipe](https://pkg.go.dev/net#Pipe) - synchronous in-memory connection for testing
