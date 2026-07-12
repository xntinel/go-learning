# 2. UDP Server and Client

UDP is connectionless: every datagram is independent, delivery is not guaranteed, and there is no per-client connection state on the server. The Go `net` package models this explicitly — a UDP server uses `net.PacketConn` with `ReadFrom`/`WriteTo` instead of `net.Listener`/`Accept`/`net.Conn`. A single socket handles all senders.

The hard part is not the API; it is closing the gap between the programmer's assumption (the datagram arrives, once, undamaged, within a bounded time) and UDP's contract (loss, reordering, duplication, and truncation are all possible). This lesson builds a reusable `udpecho` package, pins its contract with table-driven race-detector tests, and demonstrates where deadlines and size limits are non-optional.

```text
udpecho/
  go.mod
  echo.go
  echo_test.go
  cmd/demo/main.go
```

## Concepts

### Datagrams vs Streams

TCP is a byte stream: the kernel reassembles ordered, reliable segments. UDP is a datagram transport: each `WriteTo` call produces exactly one datagram at the receiver's `ReadFrom` call — or it does not arrive at all. There is no framing, no reordering, and no retransmission. A `ReadFrom` that returns `n` bytes always received the payload of one datagram, never a partial datagram and never two merged datagrams.

This boundary-preserving property is what DNS, NTP, gaming protocols, and metrics pipelines exploit. A 30-byte DNS query always fits in one datagram and gets one reply; the protocol does not need a length prefix. A TCP equivalent would require framing.

### PacketConn and the ReadFrom/WriteTo Loop

`net.ListenPacket("udp", addr)` returns a `net.PacketConn`. The interface is:

```go
type PacketConn interface {
	ReadFrom(p []byte) (n int, addr Addr, err error)
	WriteTo(p []byte, addr Addr) (n int, err error)
	Close() error
	LocalAddr() Addr
	SetDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}
```

Because there are no connections, there is no `Accept`. The server loops on `ReadFrom`, which blocks until a datagram arrives, fills `p` up to `len(p)` bytes, and returns the sender's address. The response goes back with `WriteTo(reply, addr)` using the same connection. One goroutine is enough for a request-reply pattern.

### Connected vs Unconnected UDP Sockets

`net.Dial("udp", addr)` creates a "connected" UDP socket. No handshake occurs — UDP is still connectionless — but the kernel records the remote address so you can use `Write`/`Read` instead of `WriteTo`/`ReadFrom`. An additional effect: the connected socket's `Read` only delivers datagrams arriving *from* the specified remote address. This is useful for clients that talk to one server: stray datagrams from other addresses are filtered by the kernel, not by application code.

An unconnected `PacketConn` (from `ListenPacket`) receives datagrams from any address.

### Why Deadlines Are Non-Optional

UDP provides no acknowledgement. If the server is absent, `conn.Read` on a connected client socket blocks forever — there is no RST, no FIN, no keepalive to unblock it. Set a deadline before every read:

```go
conn.SetDeadline(time.Now().Add(2 * time.Second))
```

A timed-out read returns a `net.Error` with `Timeout() == true`. Without a deadline, a lost reply hangs the goroutine indefinitely.

### The MTU Boundary and Datagram Size

Ethernet MTU is 1500 bytes. After the 20-byte IPv4 header and 8-byte UDP header, the maximum payload without IP fragmentation is 1472 bytes. Fragmented datagrams are reassembled by the kernel, but a single lost fragment causes the whole datagram to be discarded with no notification to the application. Keep payloads under 1400 bytes for a comfortable margin.

The theoretical UDP maximum is 65 507 bytes (65 535 − 20 − 8). Sending that over a LAN risks fragmentation; sending it over the internet is a reliability hazard.

## Exercises

This is a library, not a program: there is no top-level `main`. You verify it with `go test`.

### Exercise 1: The Server and the Send Helper

Create `echo.go`:

```go
package udpecho

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrDatagramTooLarge is returned by Send when msg exceeds MaxDatagramSize.
var ErrDatagramTooLarge = errors.New("datagram exceeds MaxDatagramSize")

// MaxDatagramSize is the largest payload Send will transmit and Server will
// buffer. Staying at or below this value prevents IP fragmentation on
// Ethernet-based networks (MTU 1500, minus 20-byte IPv4 and 8-byte UDP
// headers, leaves 1472; 1400 provides a comfortable margin).
const MaxDatagramSize = 1400

// Server is a connectionless UDP echo server. It echoes every received
// datagram back to the sender. A single goroutine services all senders;
// no per-client connection state is kept. Close shuts down the listener
// and the goroutine exits on the next blocked ReadFrom.
type Server struct {
	pc   net.PacketConn
	addr net.Addr
}

// New starts a UDP echo server on addr. Pass "host:0" to let the OS choose
// an ephemeral port; call Addr to retrieve the actual address.
func New(addr string) (*Server, error) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("udpecho: listen: %w", err)
	}
	s := &Server{pc: pc, addr: pc.LocalAddr()}
	go s.serve()
	return s, nil
}

func (s *Server) serve() {
	buf := make([]byte, MaxDatagramSize)
	for {
		n, from, err := s.pc.ReadFrom(buf)
		if err != nil {
			return // PacketConn closed by Close()
		}
		_, _ = s.pc.WriteTo(buf[:n], from)
	}
}

// Addr returns the local address the server is bound to.
func (s *Server) Addr() net.Addr { return s.addr }

// Close shuts down the server's listener.
func (s *Server) Close() error { return s.pc.Close() }

// Send sends msg to a UDP server at addr and returns the first datagram
// received in response, subject to timeout. Send returns ErrDatagramTooLarge
// without transmitting if len(msg) exceeds MaxDatagramSize.
func Send(addr string, msg []byte, timeout time.Duration) ([]byte, error) {
	if len(msg) > MaxDatagramSize {
		return nil, fmt.Errorf("%w: len=%d", ErrDatagramTooLarge, len(msg))
	}
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("udpecho: dial: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("udpecho: set deadline: %w", err)
	}
	if _, err := conn.Write(msg); err != nil {
		return nil, fmt.Errorf("udpecho: write: %w", err)
	}
	buf := make([]byte, MaxDatagramSize)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("udpecho: read: %w", err)
	}
	return buf[:n], nil
}
```

`New` stores the address before starting the goroutine so `Addr()` is safe to call immediately. `serve` holds a single buffer; because it processes one datagram at a time (ReadFrom → WriteTo → ReadFrom), the buffer is never accessed concurrently.

### Exercise 2: Test the Contract

Create `echo_test.go`:

```go
package udpecho

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestEcho(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msg  string
	}{
		{"short", "hi"},
		{"sentence", "the quick brown fox"},
		{"unicode", "世界"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, err := New("127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			got, err := Send(s.Addr().String(), []byte(tc.msg), 2*time.Second)
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if string(got) != tc.msg {
				t.Fatalf("echo: got %q, want %q", got, tc.msg)
			}
		})
	}
}

func TestSendRejectsOversizedDatagram(t *testing.T) {
	t.Parallel()

	big := make([]byte, MaxDatagramSize+1)
	_, err := Send("127.0.0.1:9", big, time.Second) // port 9 is discard; never reached
	if !errors.Is(err, ErrDatagramTooLarge) {
		t.Fatalf("err = %v, want ErrDatagramTooLarge", err)
	}
}

func TestConcurrentClients(t *testing.T) {
	t.Parallel()

	s, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := fmt.Sprintf("client-%d", i)
			got, err := Send(s.Addr().String(), []byte(msg), 2*time.Second)
			if err != nil {
				errs[i] = err
				return
			}
			if string(got) != msg {
				errs[i] = fmt.Errorf("got %q, want %q", got, msg)
			}
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("client %d: %v", i, e)
		}
	}
}

// ExampleNew shows the network reported by a freshly started server.
func ExampleNew() {
	s, err := New("127.0.0.1:0")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer s.Close()
	fmt.Println(s.Addr().Network())
	// Output:
	// udp
}
```

`TestConcurrentClients` exercises the single-goroutine server with 20 parallel senders. Each `Send` call uses `net.Dial("udp", ...)`, which gives every goroutine its own ephemeral source port. The server echoes back to the source address, so responses are routed to the correct caller by the kernel — no demuxing is needed in application code.

`wg.Wait()` establishes a happens-before between each goroutine's write to `errs[i]` and the main goroutine's reads, so there is no data race.

Your turn: add `TestServerAddrNetwork` that creates a server and asserts `s.Addr().Network() == "udp"`. This pins that the server is always bound to the expected network type.

### Exercise 3: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/udpecho"
)

func main() {
	s, err := udpecho.New("127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	fmt.Printf("server listening on %s\n", s.Addr())

	messages := []string{"ping", "hello UDP", "one more datagram"}
	for _, m := range messages {
		resp, err := udpecho.Send(s.Addr().String(), []byte(m), time.Second)
		if err != nil {
			log.Printf("send %q: %v", m, err)
			continue
		}
		fmt.Printf("sent %q  echo %q\n", m, string(resp))
	}
}
```

Run it with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Reusing a Buffer Across Goroutines in the Server

Wrong: dispatching the datagram to a goroutine and reusing the buffer immediately.

```go
// Wrong: buf is overwritten before the goroutine reads it.
go func() { process(buf[:n]) }()
// next ReadFrom overwrites buf here
```

The serve loop in this lesson does not dispatch — it processes one datagram synchronously before reading the next. If you add concurrent dispatch, make a copy of the relevant slice before starting the goroutine.

### Assuming a Read Returns Exactly What Was Written

Wrong: sending a 500-byte payload and expecting `Read` on the client to return exactly 500 bytes.

UDP truncates if the receive buffer is smaller than the datagram. If `len(buf) < len(datagram)`, `ReadFrom` silently discards the overflow. Size the buffer at `MaxDatagramSize` (or larger) to avoid silent data loss.

### Not Setting a Deadline on the Client Read

Wrong:

```go
n, err := conn.Read(buf) // blocks forever if the server never replies
```

Fix:

```go
conn.SetDeadline(time.Now().Add(2 * time.Second))
n, err := conn.Read(buf)
if err != nil {
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		// server did not reply in time
	}
}
```

The `Send` helper in this lesson always sets a deadline before reading. Never omit it in production code.

### Sending Oversized Datagrams

Wrong: writing a datagram larger than the path MTU (typically 1472 bytes over Ethernet).

The kernel may fragment the datagram at the IP layer. A single lost IP fragment causes the entire datagram to be silently dropped at the receiver, with no error returned to the sender. `Send` in this lesson rejects payloads above `MaxDatagramSize` (1400 bytes) to prevent this.

## Verification

From `~/go-exercises/udpecho`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. There is no program to run as the verification mechanism — `go test` is the check.

## Summary

- `net.ListenPacket("udp", addr)` returns a `net.PacketConn`; the server loops on `ReadFrom`/`WriteTo` with no `Accept` call.
- `net.Dial("udp", addr)` creates a connected client socket that filters replies by remote address; use `Write`/`Read`.
- One UDP socket handles all senders; no per-client goroutine is required for request-reply patterns.
- Always set a read deadline on the client; a lost reply blocks the goroutine indefinitely without one.
- Keep payloads at or below 1400 bytes to avoid IP fragmentation and silent data loss.
- Validate datagram size before sending and size receive buffers to `MaxDatagramSize` to prevent truncation.

## What's Next

Next: [Concurrent TCP Server](../03-concurrent-tcp-server/03-concurrent-tcp-server.md).

## Resources

- [net.ListenPacket](https://pkg.go.dev/net#ListenPacket)
- [net.PacketConn](https://pkg.go.dev/net#PacketConn)
- [net.Dial](https://pkg.go.dev/net#Dial)
- [net.Error (Timeout)](https://pkg.go.dev/net#Error)
- [RFC 768 — User Datagram Protocol](https://www.rfc-editor.org/rfc/rfc768)
