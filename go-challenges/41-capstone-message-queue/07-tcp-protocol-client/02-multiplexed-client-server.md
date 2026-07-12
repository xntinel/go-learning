# Exercise 2: Multiplexed Client and Server

One TCP connection, many in-flight requests. This exercise builds the pair that makes that work: a `Server` that accepts connections and dispatches each frame to a `Handler` concurrently, and a `Client` whose `Roundtrip` is safe to call from a hundred goroutines at once, each matched to its reply by correlation ID. It is the multiplexing core of every modern message-queue and RPC client.

This module is fully self-contained: it bundles its own minimal frame codec, starts with its own `go mod init`, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
frame.go             Frame, Encode, Decode, APIKey, ErrConnClosed (bundled minimal codec)
server.go            Server, Handler, Start/Serve/Close/Addr, handleConn
client.go            Client, Dial, Roundtrip (correlation-ID multiplexing), readLoop
cmd/
  demo/
    main.go          loopback echo server + 5 concurrent round trips on one conn
mqmux_test.go        echo over net.Pipe, 100 concurrent round trips, closed-client error,
                     full round trip over a net.Listen loopback
```

- Files: `frame.go`, `server.go`, `client.go`, `cmd/demo/main.go`, `mqmux_test.go`.
- Implement: `Server` with `Start`/`Serve`/`Close`/`Addr` and a per-frame dispatch `handleConn`; `Client` with `Dial`, `Roundtrip`, `Close`, and a single `readLoop`.
- Test: round-trip an echo over `net.Pipe`, drive 100 concurrent round trips, assert a closed client returns `ErrConnClosed`, and run one round trip over a real loopback listener.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/41-capstone-message-queue/07-tcp-protocol-client/02-multiplexed-client-server/cmd/demo && cd go-solutions/41-capstone-message-queue/07-tcp-protocol-client/02-multiplexed-client-server
```

### Why one reader, one write mutex, and a correlation-ID map

The client's job is to let many goroutines share one connection without blocking each other and without corrupting the byte stream. Three pieces make that safe, and each addresses a distinct hazard.

The write side is shared, so it needs a mutex. Multiple goroutines call `Roundtrip` at once; if two of them wrote to the connection concurrently, TCP would interleave their bytes and the server would decode a frame spliced from two requests. A single `writeMu` held across the `Encode` and the `Flush` makes each frame's emission atomic. The flush must be inside the lock: a buffered writer flushed after releasing the lock lets a second goroutine append its bytes to the buffer before the first goroutine's bytes leave it.

The read side is not shared, so it needs no mutex — but it does need to be exactly one goroutine. `readLoop` is the sole reader; TCP delivers frames in the order the server wrote them, and `readLoop` decodes them one at a time. For each response it pulls the correlation ID, looks up the waiting caller's channel in the `pending` map, deletes the entry, and delivers the frame. That map is the multiplexer: it is the only thing connecting a response that arrived on the shared connection back to the specific goroutine that is blocked waiting for it.

The correlation ID itself comes from an `atomic.Int32` counter, incremented per request so two concurrent callers never collide on an ID. A buffered `sema` channel of size `maxInFlight` bounds how many requests can be outstanding: a caller acquires a slot before sending and releases it after receiving, so the connection cannot accumulate unbounded pending entries under load. On any send error the caller must delete its `pending` entry before returning, or the map and its channel leak until the connection closes.

The server mirrors this. `handleConn` reads frames in a single loop and spawns a goroutine per frame to run the `Handler`, so a slow handler does not stall the others. Responses may therefore leave out of order — which is exactly why the client matches by correlation ID rather than by position. A per-connection `writeMu` serializes the handler goroutines' writes, an inner `sync.WaitGroup` drained by `defer wg.Wait()` ensures the connection does not close while a handler is still writing, and a read deadline keeps an idle connection from pinning a goroutine forever.

Create `frame.go`:

```go
package mqmux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// APIKey identifies the protocol operation carried in a Frame header.
type APIKey uint16

const (
	APIProduce APIKey = 0
	APIFetch   APIKey = 1
)

// Sentinel errors surfaced by the client.
var (
	ErrBadFrame   = errors.New("malformed frame")
	ErrConnClosed = errors.New("connection closed")
)

// headerLen is the fixed body prefix after the 4-byte length field:
// [2] API key, [2] API version, [4] correlation ID.
const headerLen = 8

// Frame is one protocol message, request or response. The length field counts
// the bytes after itself (Kafka convention): length = headerLen + len(Payload).
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
		return fmt.Errorf("mqmux: write header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("mqmux: write payload: %w", err)
		}
	}
	return nil
}

// Decode reads exactly one frame from r using io.ReadFull for both the length
// and the body, so a frame split across TCP segments still decodes.
func Decode(r io.Reader) (*Frame, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("mqmux: read length: %w", err)
	}
	length := int(binary.BigEndian.Uint32(lenBuf[:]))
	if length < headerLen {
		return nil, fmt.Errorf("%w: length=%d", ErrBadFrame, length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("mqmux: read frame body: %w", err)
	}
	return &Frame{
		APIKey:        APIKey(binary.BigEndian.Uint16(body[0:])),
		APIVersion:    binary.BigEndian.Uint16(body[2:]),
		CorrelationID: int32(binary.BigEndian.Uint32(body[4:])),
		Payload:       body[headerLen:],
	}, nil
}
```

Create `server.go`:

```go
package mqmux

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	defaultReadDeadline  = 30 * time.Second
	defaultWriteDeadline = 10 * time.Second
)

// Handler processes one request frame and returns a response frame. The
// response must carry the same CorrelationID as the request. Handler may be
// called concurrently from multiple goroutines.
type Handler func(req *Frame) *Frame

// Server listens for TCP connections and dispatches frames to a Handler.
type Server struct {
	addr    string
	handler Handler
	logger  *slog.Logger

	mu sync.Mutex
	ln net.Listener

	wg   sync.WaitGroup
	done chan struct{}
}

// NewServer creates a Server that calls handler for every inbound frame.
func NewServer(addr string, handler Handler) *Server {
	return &Server{
		addr:    addr,
		handler: handler,
		logger:  slog.Default(),
		done:    make(chan struct{}),
	}
}

// Start opens the TCP listener without blocking. Call Serve afterwards. The
// split lets a caller read Addr (useful with port 0) before accepting begins.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("mqmux server: listen: %w", err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	return nil
}

// Serve accepts connections until Close is called. Call Start first.
func (s *Server) Serve() {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				s.logger.Error("accept", "err", err)
				continue
			}
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// Addr returns the listener's address once Start has been called.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Close stops the listener and waits for all connection handlers to exit.
func (s *Server) Close() error {
	close(s.done)
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	var writeMu sync.Mutex

	// wg tracks in-flight handler goroutines so a graceful close drains them.
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn.SetReadDeadline(time.Now().Add(defaultReadDeadline))
		req, err := Decode(br)
		if err != nil {
			return
		}
		wg.Add(1)
		go func(req *Frame) {
			defer wg.Done()
			resp := s.handler(req)
			if resp == nil {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(defaultWriteDeadline))
			writeMu.Lock()
			defer writeMu.Unlock()
			if err := resp.Encode(bw); err != nil {
				s.logger.Error("write response", "err", err)
				return
			}
			bw.Flush()
		}(req)
	}
}
```

Create `client.go`:

```go
package mqmux

import (
	"bufio"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
)

// maxInFlight bounds concurrent unacknowledged requests. It acts as a
// semaphore: the next caller blocks once this many requests are outstanding.
const maxInFlight = 256

// Client connects to a single broker over TCP and multiplexes concurrent
// requests by assigning each a unique CorrelationID. Multiple goroutines may
// call Roundtrip concurrently without external locking.
type Client struct {
	conn    net.Conn
	bw      *bufio.Writer
	writeMu sync.Mutex

	pendMu  sync.Mutex
	pending map[int32]chan *Frame

	nextID atomic.Int32
	sema   chan struct{}

	once sync.Once
	done chan struct{}
}

// Dial creates a Client connected to addr.
func Dial(addr string) (*Client, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mqmux client: dial %s: %w", addr, err)
	}
	c := &Client{
		conn:    conn,
		bw:      bufio.NewWriter(conn),
		pending: make(map[int32]chan *Frame),
		sema:    make(chan struct{}, maxInFlight),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Close shuts down the client. In-flight Roundtrip calls return ErrConnClosed.
func (c *Client) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.conn.Close()
}

// Roundtrip sends req and blocks until the server returns a matching response.
// It assigns a unique CorrelationID. Safe for concurrent use.
func (c *Client) Roundtrip(req *Frame) (*Frame, error) {
	// Fast path: a client already closed must not attempt a send. Checked first
	// (rather than only in the select below) because the select picks randomly
	// when both the semaphore and c.done are ready.
	select {
	case <-c.done:
		return nil, ErrConnClosed
	default:
	}
	select {
	case c.sema <- struct{}{}:
	case <-c.done:
		return nil, ErrConnClosed
	}
	defer func() { <-c.sema }()

	id := c.nextID.Add(1)
	req.CorrelationID = id

	ch := make(chan *Frame, 1)
	c.pendMu.Lock()
	c.pending[id] = ch
	c.pendMu.Unlock()

	c.writeMu.Lock()
	err := req.Encode(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}
	c.writeMu.Unlock()

	if err != nil {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return nil, fmt.Errorf("mqmux client: send: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-c.done:
		return nil, ErrConnClosed
	}
}

// readLoop is the single reader goroutine. TCP delivers frames in order, so the
// read side needs no mutex. Each response is dispatched by CorrelationID.
func (c *Client) readLoop() {
	br := bufio.NewReader(c.conn)
	for {
		f, err := Decode(br)
		if err != nil {
			c.once.Do(func() { close(c.done) })
			return
		}
		c.pendMu.Lock()
		ch, ok := c.pending[f.CorrelationID]
		if ok {
			delete(c.pending, f.CorrelationID)
		}
		c.pendMu.Unlock()
		if ok {
			ch <- f
		}
	}
}
```

Trace one `Roundtrip`. It takes a semaphore slot (or returns `ErrConnClosed` if the client is shutting down), claims the next correlation ID, registers a 1-buffered reply channel in `pending`, then writes the frame under `writeMu`. If the write fails it removes its `pending` entry so nothing leaks, otherwise it blocks selecting on its reply channel and `c.done`. Meanwhile `readLoop` decodes whatever the server sent, finds the matching channel by correlation ID, and delivers the frame; the 1-slot buffer means the send never blocks even if the caller has not yet reached its receive. Close the client and `readLoop`'s next `Decode` fails, which closes `done` and releases every waiter with `ErrConnClosed`.

### The runnable demo

The demo starts an echo server on a loopback port, dials it, and fires five requests concurrently over the single connection. It stores each reply at its index so the printed output is ordered regardless of the non-deterministic completion order — the point being that five goroutines shared one connection and every reply found its caller.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"sync"

	"example.com/mqmux"
)

func main() {
	srv := mqmux.NewServer("127.0.0.1:0", func(req *mqmux.Frame) *mqmux.Frame {
		return &mqmux.Frame{
			APIKey:        req.APIKey,
			APIVersion:    req.APIVersion,
			CorrelationID: req.CorrelationID,
			Payload:       req.Payload,
		}
	})
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	c, err := mqmux.Dial(srv.Addr().String())
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	const n = 5
	replies := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := fmt.Appendf(nil, "message-%d", i)
			resp, err := c.Roundtrip(&mqmux.Frame{APIKey: mqmux.APIProduce, Payload: payload})
			if err != nil {
				log.Printf("request %d: %v", i, err)
				return
			}
			replies[i] = string(resp.Payload)
		}(i)
	}
	wg.Wait()

	fmt.Printf("sent %d concurrent requests on one connection\n", n)
	for i, r := range replies {
		fmt.Printf("  reply[%d] = %q\n", i, r)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sent 5 concurrent requests on one connection
  reply[0] = "message-0"
  reply[1] = "message-1"
  reply[2] = "message-2"
  reply[3] = "message-3"
  reply[4] = "message-4"
```

### Tests

`newPipeClient` wires a `Client` directly to a server's `handleConn` over `net.Pipe`, so the multiplexing logic is tested with no real socket. `TestRoundtripEcho` checks one request, `TestConcurrentRoundtrips` drives 100 at once (the `-race` flag turns any missing synchronization into a failure here), and `TestClientClosedReturnsErrConnClosed` asserts a closed client surfaces the sentinel. `TestLoopbackRoundtrip` runs the whole stack — `Dial` over a real `net.Listen` loopback — end to end.

Create `mqmux_test.go`:

```go
package mqmux

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
)

func echoHandler(req *Frame) *Frame {
	return &Frame{
		APIKey:        req.APIKey,
		APIVersion:    req.APIVersion,
		CorrelationID: req.CorrelationID,
		Payload:       append([]byte(nil), req.Payload...),
	}
}

// newPipeClient wires a Client and a server handleConn together over net.Pipe,
// so no real network is required. Cleanup closes the client and drains the
// server goroutine.
func newPipeClient(t *testing.T, h Handler) *Client {
	t.Helper()
	srv := NewServer("", h)
	clientConn, serverConn := net.Pipe()
	srv.wg.Add(1)
	go srv.handleConn(serverConn)

	c := &Client{
		conn:    clientConn,
		bw:      bufio.NewWriter(clientConn),
		pending: make(map[int32]chan *Frame),
		sema:    make(chan struct{}, maxInFlight),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	t.Cleanup(func() {
		c.Close()
		srv.wg.Wait()
	})
	return c
}

func TestRoundtripEcho(t *testing.T) {
	t.Parallel()
	c := newPipeClient(t, echoHandler)

	req := &Frame{APIKey: APIProduce, Payload: []byte("test-payload")}
	resp, err := c.Roundtrip(req)
	if err != nil {
		t.Fatalf("Roundtrip: %v", err)
	}
	if resp.CorrelationID != req.CorrelationID {
		t.Errorf("correlation mismatch: got %d, want %d", resp.CorrelationID, req.CorrelationID)
	}
	if !bytes.Equal(resp.Payload, []byte("test-payload")) {
		t.Errorf("payload mismatch: got %q", resp.Payload)
	}
}

func TestConcurrentRoundtrips(t *testing.T) {
	t.Parallel()
	c := newPipeClient(t, echoHandler)

	const n = 100
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := fmt.Appendf(nil, "msg-%d", i)
			resp, err := c.Roundtrip(&Frame{APIKey: APIProduce, Payload: payload})
			if err != nil {
				errs[i] = err
				return
			}
			if !bytes.Equal(resp.Payload, payload) {
				errs[i] = fmt.Errorf("goroutine %d: got %q, want %q", i, resp.Payload, payload)
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Errorf("%v", err)
		}
	}
}

func TestClientClosedReturnsErrConnClosed(t *testing.T) {
	t.Parallel()
	c := newPipeClient(t, echoHandler)
	c.Close()

	_, err := c.Roundtrip(&Frame{APIKey: APIProduce, Payload: []byte("x")})
	if !errors.Is(err, ErrConnClosed) {
		t.Fatalf("err = %v, want ErrConnClosed", err)
	}
}

func TestLoopbackRoundtrip(t *testing.T) {
	t.Parallel()
	srv := NewServer("127.0.0.1:0", echoHandler)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })

	c, err := Dial(srv.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	resp, err := c.Roundtrip(&Frame{APIKey: APIFetch, Payload: []byte("loopback")})
	if err != nil {
		t.Fatalf("Roundtrip: %v", err)
	}
	if !bytes.Equal(resp.Payload, []byte("loopback")) {
		t.Errorf("payload mismatch: got %q", resp.Payload)
	}
}
```

## Review

The pair is correct when writes are serialized, reads are single-goroutine, and every response finds its caller. Confirm `writeMu` wraps both the `Encode` and the `Flush` on each side, that `readLoop` is the only reader, and that a send error deletes the `pending` entry before returning so neither the map nor its channel leaks. The `-race` run of `TestConcurrentRoundtrips` is the real proof: 100 goroutines sharing one connection with any missing synchronization shows up immediately.

Common mistakes for this feature. The first is flushing outside the write mutex, which lets a second goroutine's bytes interleave with the first's in the buffered writer. The second is failing to copy the correlation ID from request to response in the handler, after which the client's reader looks up ID 0, finds nothing, and the caller blocks until its deadline. The third is closing the connection while a handler goroutine is still writing; `defer wg.Wait()` inside `handleConn` is the drain that prevents it, and putting the wait after the read loop's `return` would make it unreachable.

## Resources

- [`net` package](https://pkg.go.dev/net) — `Dial`, `Listen`, `Conn`, and `Pipe`, the connection primitives this client and server are built on and tested with.
- [`net/rpc` Client.Go](https://pkg.go.dev/net/rpc#Client.Go) — the standard library's own correlation-ID multiplexer; its `pending map[uint64]*Call` is the same pattern as the `pending` map here.
- [Kafka Protocol Guide](https://kafka.apache.org/protocol.html) — the correlation-ID request/response matching this multiplexer implements.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-request-pipelining.md](03-request-pipelining.md)
