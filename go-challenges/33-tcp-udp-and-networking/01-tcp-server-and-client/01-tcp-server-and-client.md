# 1. TCP Server and Client

`net.Listen` and `net.Dial` surface TCP networking through the same `io.Reader`/`io.Writer` interface that the rest of the standard library uses, but the connection model has real edges: `Read` returns short reads, every accepted connection must be closed exactly once, and tests that start a real TCP listener must use port 0 rather than a fixed address. This lesson builds a small `tcpecho` package — a server type that echoes bytes back to any client — and tests it in-process with no external network dependency.

```text
tcpecho/
  go.mod
  server.go
  server_test.go
  cmd/demo/main.go
```

The package exports a `Server` type that wraps a `net.Listener`. Tests bind to `127.0.0.1:0` and retrieve the assigned port from `listener.Addr()`.

## Concepts

### The TCP Connection Model

A TCP connection is a full-duplex byte stream. The server calls `net.Listen` to create a `net.Listener` bound to an address, then calls `listener.Accept()` in a loop. `Accept` blocks until a client connects; it returns a `net.Conn` representing that connection. The client calls `net.Dial`, which completes the three-way handshake and returns its own `net.Conn`. From that point both sides read and write on their respective `net.Conn` values until one side closes.

The "byte stream" part matters for correctness: `conn.Read(buf)` may return fewer bytes than `len(buf)` even when more data is in flight. TCP segments data at the kernel level; a single `Write("hello, world")` on one end may arrive as two separate `Read` calls on the other. Use `io.ReadFull` for fixed-size reads, or wrap the connection in `bufio.Scanner` for newline-delimited messages.

### `net.Listen`, `Accept`, and the Connection Loop

```go
listener, err := net.Listen("tcp", "127.0.0.1:0")
```

The first argument is the network name: `"tcp"`, `"tcp4"`, or `"tcp6"`. Port `0` asks the OS to assign any available port; `listener.Addr().String()` retrieves the full `host:port` afterward.

The standard accept loop:

```go
for {
	conn, err := listener.Accept()
	if err != nil {
		return // listener was closed
	}
	go handle(conn)
}
```

Each connection needs its own goroutine because `conn.Read` blocks until data arrives. Omitting `go` makes the server single-threaded: it cannot accept the next connection while processing the current one.

### `net.Conn` as `io.Reader` and `io.Writer`

`net.Conn` satisfies both `io.Reader` and `io.Writer`. Any stdlib function that accepts an `io.Reader` — `bufio.NewReader`, `io.ReadFull`, `json.NewDecoder` — and any that accepts an `io.Writer` — `fmt.Fprintf`, `io.Copy`, `bufio.NewWriter` — work directly on a connection without adapters.

### `io.Copy` for Echo

```go
io.Copy(conn, conn)
```

`io.Copy(dst, src)` reads from `src` and writes to `dst`. Passing the same `net.Conn` for both arguments reads what the remote side sent and writes it back — an echo loop that needs no manual buffer management. `io.Copy` allocates a 32 KB internal buffer and loops until `src` returns `io.EOF`. For a TCP connection, that happens when the remote side closes its write half (via a full close or `conn.(*net.TCPConn).CloseWrite()`).

### Port 0 and Hermetic Tests

`net.Listen("tcp", "127.0.0.1:0")` asks the OS for any free port. Tests that hardcode `:9000` fail whenever another process holds that port or two tests run in parallel and collide. With port 0, the OS guarantees uniqueness and no external network is needed. `listener.Addr().String()` returns the actual `host:port` the test should dial.

### Half-Close: the Missing Piece for `io.ReadAll`

A test that writes to the server and then calls `io.ReadAll` deadlocks without a half-close. `io.ReadAll` reads until `io.EOF`. The server's `io.Copy` also blocks waiting for `io.EOF` from the client. Neither side closes first; both wait forever.

The fix is a TCP half-close: the client signals "no more data" while keeping its read side open:

```go
conn.(*net.TCPConn).CloseWrite()
```

This sends a TCP FIN. The server's `io.Copy` sees `io.EOF` on its next `Read`, writes the buffered echo, returns, and the server handler closes the connection. The client's `io.ReadAll` then sees `io.EOF` and returns.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/33-tcp-udp-and-networking/01-tcp-server-and-client/01-tcp-server-and-client/cmd/demo
cd go-solutions/33-tcp-udp-and-networking/01-tcp-server-and-client/01-tcp-server-and-client
```

### Exercise 1: The EchoServer Type

Create `server.go`:

```go
package tcpecho

import (
	"io"
	"net"
)

// Server is a TCP echo server. Any bytes received from a client are written
// back to the same client before the connection is closed.
type Server struct {
	listener net.Listener
}

// NewServer creates a TCP listener bound to addr.
// Pass "127.0.0.1:0" in tests to let the OS assign a free port.
func NewServer(addr string) (*Server, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Server{listener: l}, nil
}

// Addr returns the address the server is listening on, e.g. "127.0.0.1:51234".
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Serve accepts connections in a loop and echoes each in its own goroutine.
// It returns when the listener is closed.
func (s *Server) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go echo(conn)
	}
}

// Close shuts down the listener. In-flight connections finish normally.
func (s *Server) Close() error {
	return s.listener.Close()
}

func echo(conn net.Conn) {
	defer conn.Close()
	io.Copy(conn, conn)
}
```

`echo` passes the same `net.Conn` as both the `io.Copy` destination and source. Because `net.Conn` is full-duplex, reading from it yields what the remote side sent; writing to it sends data back. No separate buffer is needed.

### Exercise 2: Tests and an Example

Create `server_test.go`:

```go
package tcpecho

import (
	"fmt"
	"io"
	"net"
	"testing"
)

// startTestServer starts an echo server on a random port and registers
// a cleanup to close it when the test finishes.
func startTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestEchoRoundTrip(t *testing.T) {
	t.Parallel()
	srv := startTestServer(t)

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	const want = "hello"
	if _, err := io.WriteString(conn, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Half-close: tell the server there is no more data.
	// Without this, both sides block waiting for the other to close first.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != want {
		t.Errorf("echo = %q, want %q", string(got), want)
	}
}

func TestEchoMultipleClients(t *testing.T) {
	t.Parallel()
	srv := startTestServer(t)

	for _, msg := range []string{"alpha", "beta", "gamma"} {
		t.Run(msg, func(t *testing.T) {
			t.Parallel()
			conn, err := net.Dial("tcp", srv.Addr())
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer conn.Close()

			if _, err := io.WriteString(conn, msg); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatalf("CloseWrite: %v", err)
			}
			got, err := io.ReadAll(conn)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(got) != msg {
				t.Errorf("echo = %q, want %q", string(got), msg)
			}
		})
	}
}

func TestAddrIsNonEmpty(t *testing.T) {
	t.Parallel()
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.Addr() == "" {
		t.Error("Addr() must not be empty after NewServer")
	}
}

func TestCloseStopsAccept(t *testing.T) {
	t.Parallel()
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := srv.Addr()
	srv.Close()

	_, err = net.Dial("tcp", addr)
	if err == nil {
		t.Error("Dial after Close should return an error")
	}
}

func ExampleServer() {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		return
	}
	defer srv.Close()
	go srv.Serve()

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		return
	}
	defer conn.Close()

	io.WriteString(conn, "ping")
	conn.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(conn)
	fmt.Println(string(got))
	// Output:
	// ping
}
```

Your turn: add `TestEchoLargePayload` — write a 64 KB message (make a `[]byte` of 65536 bytes, fill it with `'x'`), send it, half-close, read all, and assert the round-trip length is 65536.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"net"

	"example.com/tcpecho"
)

func main() {
	srv, err := tcpecho.NewServer("127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	go srv.Serve()
	fmt.Println("server listening on", srv.Addr())

	messages := []string{"hello", "world", "echo"}
	for _, m := range messages {
		conn, err := net.Dial("tcp", srv.Addr())
		if err != nil {
			log.Fatal(err)
		}
		if _, err := io.WriteString(conn, m); err != nil {
			conn.Close()
			log.Fatal(err)
		}
		if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
			conn.Close()
			log.Fatal(err)
		}
		got, err := io.ReadAll(conn)
		conn.Close()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("sent %q  received %q\n", m, string(got))
	}
}
```

Run with:

```bash
go run ./cmd/demo
```

The demo opens a fresh connection per message to keep the read side simple: each connection carries exactly one message and one echo.

## Common Mistakes

### Treating `Read` as a Full Receive

Wrong: reading without using `n`, assuming the whole buffer was filled.

```go
buf := make([]byte, 1024)
conn.Read(buf)
fmt.Println(string(buf)) // prints 1016 garbage zero bytes
```

What happens: `Read` returns as soon as any data arrives; the rest of `buf` is zeroed from `make`. A 5-byte message fills only `buf[:5]`.

Fix: use the returned `n`, or use `io.ReadFull` when you know the exact byte count:

```go
n, err := conn.Read(buf)
fmt.Println(string(buf[:n]))
```

### Fixed Port in Tests

Wrong:

```go
listener, _ := net.Listen("tcp", ":9000")
```

What happens: the test fails when another process holds port 9000, and two parallel subtests collide and both get "address already in use".

Fix: `"127.0.0.1:0"` — the OS picks a free port and `listener.Addr().String()` returns it.

### Deadlock from Missing `CloseWrite`

Wrong: writing to the server and immediately calling `io.ReadAll` without a half-close.

```go
io.WriteString(conn, "hello")
got, _ := io.ReadAll(conn) // hangs forever
```

What happens: `io.ReadAll` blocks until `io.EOF`. The server's `io.Copy` also blocks waiting for `io.EOF` from the client. Neither side proceeds — a deadlock.

Fix: call `conn.(*net.TCPConn).CloseWrite()` after writing. It sends a TCP FIN, signaling end-of-stream to the server without closing the client's read side.

### Leaking the Connection File Descriptor

Wrong: returning from the handler goroutine without closing the connection.

```go
go func(c net.Conn) {
	io.Copy(c, c)
}(conn)
```

What happens: the file descriptor for the connection is never released. Under load this exhausts the OS per-process file descriptor limit and causes new connections to fail.

Fix: `defer conn.Close()` is the first statement inside every handler.

## Verification

From `~/go-exercises/tcpecho`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The tests are the verification — there is no output to eyeball.

## Summary

- `net.Listen("tcp", "127.0.0.1:0")` binds a listener; port 0 lets the OS assign a free port.
- `listener.Accept()` blocks until a client connects and returns a `net.Conn`.
- Each accepted connection runs in its own goroutine; `defer conn.Close()` is the first statement.
- `net.Conn` implements `io.Reader` and `io.Writer`; `io.Copy(conn, conn)` is the idiomatic echo loop.
- `Read` may return fewer bytes than the buffer; use `io.ReadFull` for fixed-size reads.
- Tests use `conn.(*net.TCPConn).CloseWrite()` before `io.ReadAll` to avoid deadlock.

## What's Next

Next: [UDP Server and Client](../02-udp-server-and-client/02-udp-server-and-client.md).

## Resources

- [net package](https://pkg.go.dev/net) — `Listen`, `Dial`, `Conn`, `TCPConn.CloseWrite`
- [io package](https://pkg.go.dev/io) — `Copy`, `ReadFull`, `ReadAll`, `WriteString`
- [bufio package](https://pkg.go.dev/bufio) — `Scanner` for newline-framed protocols over TCP
- [Effective Go: Goroutines](https://go.dev/doc/effective_go#goroutines) — the connection-per-goroutine pattern
- [Go Blog: Errors are values](https://go.dev/blog/errors-are-values) — handling connection errors without repetition
