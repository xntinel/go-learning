# 24. QUIC Transport Protocol

QUIC solves three structural problems that TCP cannot fix without breaking its own invariants: head-of-line blocking within a multiplexed connection, the two-to-three round-trip cost of TCP + TLS handshakes, and the binding of a connection to a fixed IP four-tuple. The hard parts of using QUIC in Go are understanding the stream lifecycle (open, write, close-send, read-response, how errors cancel only one stream), wiring TLS 1.3 with ALPN correctly (QUIC makes both mandatory), and knowing when 0-RTT data is safe versus when the server will reject it.

This lesson builds a small request/response library on top of `github.com/quic-go/quic-go` (v0.60.0). The library uses bidirectional streams as single-use channels: one request per stream, length-prefixed framing, clean half-close signaling. The framing layer is pure Go and tested offline; the QUIC network layer is tested over loopback.

```text
quictransport/
  go.mod
  frame.go                 (FrameMessage, ParseMessage — pure Go, no external deps)
  server.go                (Server — accepts connections, dispatches streams to a Handler)
  client.go                (Client — one connection, one stream per request)
  tls.go                   (GenerateSelfSigned — test and demo helper)
  quictransport_test.go    (*_test.go: framing unit tests + loopback integration tests)
  cmd/demo/main.go         (runnable demo: concurrent requests over a single connection)
```

## Concepts

### Why TCP Cannot Fix Head-of-Line Blocking

TCP delivers a byte stream in order. When a packet is lost, every byte after it waits, regardless of which application-level message the bytes belong to. HTTP/2 multiplexes logical streams over one TCP connection for connection reuse but inherits the same problem: a single lost packet stalls every HTTP/2 stream. Pooling TCP connections (lesson 07 of this chapter) partially mitigates this but adds resource and complexity overhead.

QUIC runs over UDP and assigns each stream its own loss-recovery context. A lost packet on stream 3 does not block reads on streams 4 or 5. The connection-level loss recovery still applies (acknowledgments, congestion control), but it never imposes order across stream boundaries.

### Integrated Handshake: 1-RTT and 0-RTT

TCP requires one round trip to establish the connection, then TLS 1.3 requires one more round trip. QUIC integrates the transport and crypto handshakes: the client sends its crypto hello in the first UDP datagram, and the server can reply with application data in the same flight. First-connection latency is 1 RTT instead of 2-3.

For repeat connections to the same server, QUIC supports 0-RTT resumption. The client sends application data in the initial datagram using keying material from a previous session ticket. The server can begin processing before the handshake completes. The trade-off is that 0-RTT data is not forward-secret (it uses an earlier session key) and is vulnerable to replay attacks. The server must treat 0-RTT data as non-idempotent until the handshake is confirmed, and the server can reject 0-RTT at any time, in which case the client retransmits the data over the 1-RTT connection.

In quic-go: enable 0-RTT on the server with `Allow0RTT: true` in `*quic.Config`, listen with `quic.ListenAddrEarly`, and dial with `quic.DialAddrEarly` on the client. The client TLS config must include a `ClientSessionCache` so the session ticket is stored between connections.

### QUIC Requires TLS 1.3 and ALPN

Unlike TCP, a QUIC connection without TLS 1.3 does not exist. The crypto handshake is part of the protocol, not layered on top. Consequence: every QUIC server needs a certificate. In tests and local demos, a self-signed ECDSA certificate works; only the client's root CA pool needs updating.

QUIC also requires ALPN (`NextProtos` in `tls.Config`). If the server and client advertise no common ALPN, the TLS handshake fails. Always set `NextProtos` on both sides. HTTP/3 uses `"h3"`; this lesson uses `"quictransport/1"`.

### Stream Lifecycle

A `*quic.Stream` is bidirectional. Each side has an independent half: the write half and the read half. `stream.Write(p)` sends data. `stream.Close()` closes the write half (sends a QUIC FIN), signaling to the peer that no more data will arrive on this stream. The read half remains open until the peer closes their write half. `stream.CancelWrite(code)` aborts the write half immediately with an application error code. `stream.CancelRead(code)` discards buffered data and asks the peer to stop sending.

The request/response pattern used in this lesson:

1. Client opens a stream, writes the framed request, calls `Close()` to signal end of request.
2. Server reads the full request (the length prefix makes this unambiguous), processes it, writes the framed response, returns (which triggers `defer st.Close()`).
3. Client reads the response and finishes.

Closing only the write half on step 1 lets the client start reading the response while the server is still processing, but in this lesson the server writes the full response before closing, so the ordering is sequential per stream.

### Connection Migration

QUIC connections are identified by a connection ID chosen by the endpoints, not by the IP four-tuple. When a mobile client changes from WiFi to cellular, the connection ID is unchanged. The server sends a path-validation frame; once validated, the connection migrates without interruption. TCP connections break on IP change.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/quictransport/cmd/demo
cd ~/go-exercises/quictransport
go mod init example.com/quictransport
go get github.com/quic-go/quic-go@v0.60.0
```

### Exercise 1: The Framing Layer

Every QUIC stream carries a raw byte sequence with no implicit message boundaries. A length-prefix frame makes message boundaries explicit and keeps the protocol self-describing.

Create `frame.go`:

```go
package quictransport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrFrameTooLarge is returned when the declared frame size exceeds maxFrameSize.
var ErrFrameTooLarge = errors.New("quictransport: frame too large")

// maxFrameSize is the largest message payload accepted by ParseMessage (16 MiB).
const maxFrameSize = 16 << 20

// FrameMessage encodes payload as a 4-byte big-endian length prefix followed by
// the payload bytes. The returned slice is ready to write to a quic.Stream.
// It panics if len(payload) > maxFrameSize.
func FrameMessage(payload []byte) []byte {
	if len(payload) > maxFrameSize {
		panic(fmt.Sprintf("quictransport: FrameMessage: payload %d bytes exceeds max %d", len(payload), maxFrameSize))
	}
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(payload)))
	copy(buf[4:], payload)
	return buf
}

// ParseMessage reads one length-prefixed message from r.
// It returns ErrFrameTooLarge if the declared size exceeds maxFrameSize.
// It returns io.ErrUnexpectedEOF if the stream closes before all bytes arrive.
func ParseMessage(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("quictransport: read frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("quictransport: read frame payload: %w", err)
	}
	return payload, nil
}
```

### Exercise 2: The Server

Create `server.go`:

```go
package quictransport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"

	"github.com/quic-go/quic-go"
)

// Handler processes one request payload and returns a response payload.
// A non-nil error causes the stream to be reset with application error code 1.
type Handler func(ctx context.Context, req []byte) ([]byte, error)

// Server accepts QUIC connections and dispatches each bidirectional stream
// to a Handler. Each connection is served in its own goroutine; each stream
// within a connection is also served in its own goroutine.
type Server struct {
	listener *quic.Listener
	handler  Handler
}

// NewServer creates a QUIC listener on addr with the given TLS config and
// returns a Server. The caller must set tlsConf.NextProtos to a non-empty
// slice (for example, []string{"quictransport/1"}).
func NewServer(addr string, tlsConf *tls.Config, handler Handler) (*Server, error) {
	ln, err := quic.ListenAddr(addr, tlsConf, &quic.Config{
		MaxIncomingStreams: 1000,
		MaxIdleTimeout:     0, // no idle timeout in tests; callers may set via wrapper
	})
	if err != nil {
		return nil, fmt.Errorf("quictransport: listen %s: %w", addr, err)
	}
	return &Server{listener: ln, handler: handler}, nil
}

// Addr returns the network address the server is listening on.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Serve accepts connections until ctx is cancelled or Close is called.
// It returns nil on clean shutdown and an error on unexpected failures.
func (s *Server) Serve(ctx context.Context) error {
	for {
		conn, err := s.listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // context cancelled: clean shutdown
			}
			return fmt.Errorf("quictransport: accept connection: %w", err)
		}
		go s.serveConn(ctx, conn)
	}
}

// Close shuts down the listener immediately.
func (s *Server) Close() error { return s.listener.Close() }

func (s *Server) serveConn(ctx context.Context, conn *quic.Conn) {
	defer conn.CloseWithError(0, "done")
	var wg sync.WaitGroup
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			break // connection closed or reset: stop accepting streams
		}
		wg.Add(1)
		go func(st *quic.Stream) {
			defer wg.Done()
			s.serveStream(ctx, st)
		}(stream)
	}
	wg.Wait()
}

func (s *Server) serveStream(ctx context.Context, st *quic.Stream) {
	defer st.Close()

	req, err := ParseMessage(st)
	if err != nil {
		st.CancelRead(1)
		return
	}
	resp, err := s.handler(ctx, req)
	if err != nil {
		st.CancelWrite(1)
		return
	}
	_, _ = st.Write(FrameMessage(resp))
}
```

`serveStream` reads exactly one framed request and writes exactly one framed response. The `defer st.Close()` closes the write half on exit, signaling to the client that the response is complete. If the handler returns an error, `CancelWrite` aborts the stream and the client receives a stream reset error instead of a partial response.

### Exercise 3: The Client

Create `client.go`:

```go
package quictransport

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/quic-go/quic-go"
)

// Client holds a single QUIC connection and opens one new stream per request.
// A single QUIC connection supports thousands of concurrent streams; there is
// no need for a connection pool.
type Client struct {
	conn *quic.Conn
}

// Dial establishes a 1-RTT QUIC connection to addr.
// The caller must set tlsConf.NextProtos to match the server.
func Dial(ctx context.Context, addr string, tlsConf *tls.Config) (*Client, error) {
	conn, err := quic.DialAddr(ctx, addr, tlsConf, &quic.Config{})
	if err != nil {
		return nil, fmt.Errorf("quictransport: dial %s: %w", addr, err)
	}
	return &Client{conn: conn}, nil
}

// Request opens a new QUIC stream, sends payload, and returns the response.
// Each call uses a fresh stream. Concurrent calls are safe: each stream is
// independent — a slow or failing stream does not affect other streams.
func (c *Client) Request(ctx context.Context, payload []byte) ([]byte, error) {
	stream, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("quictransport: open stream: %w", err)
	}

	// Write the framed request.
	if _, err := stream.Write(FrameMessage(payload)); err != nil {
		stream.CancelWrite(1)
		return nil, fmt.Errorf("quictransport: write request: %w", err)
	}
	// Close the write half. The server's ParseMessage uses the length prefix
	// and does not need EOF, but closing signals that no more data is coming,
	// which lets the server process without waiting for a deadline.
	if err := stream.Close(); err != nil {
		return nil, fmt.Errorf("quictransport: close send side: %w", err)
	}

	// Read the framed response. The read half is still open after Close().
	resp, err := ParseMessage(stream)
	if err != nil {
		stream.CancelRead(1)
		return nil, fmt.Errorf("quictransport: read response: %w", err)
	}
	return resp, nil
}

// Close terminates the connection gracefully with application code 0.
func (c *Client) Close() error {
	return c.conn.CloseWithError(0, "done")
}
```

### Exercise 4: TLS Helper and Tests

Self-signed TLS certificates are sufficient for loopback tests. QUIC requires TLS 1.3; the `tls.Config` produced by this helper enforces it via `MinVersion`.

Create `tls.go`:

```go
package quictransport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"time"
)

// GenerateSelfSigned returns a server TLS config with a fresh self-signed
// ECDSA P-256 certificate and a client TLS config that trusts exactly that
// certificate. Both configs advertise the given ALPN. This is suitable for
// tests and local demos; never use self-signed certificates in production.
func GenerateSelfSigned(alpn string) (serverConf, clientConf *tls.Config, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "quictransport-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	tlsCert := tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key}

	serverConf = &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{alpn},
		MinVersion:   tls.VersionTLS13,
	}
	clientConf = &tls.Config{
		RootCAs:    pool,
		ServerName: "127.0.0.1",
		NextProtos: []string{alpn},
		MinVersion: tls.VersionTLS13,
	}
	return serverConf, clientConf, nil
}
```

Create `quictransport_test.go`:

```go
package quictransport

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ---- framing unit tests (no network, run offline) ----

func TestFrameMessageRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload []byte
	}{
		{"empty", []byte{}},
		{"short", []byte("hello")},
		{"binary", []byte{0x00, 0xFF, 0x80, 0x01}},
		{"64k", make([]byte, 64*1024)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			frame := FrameMessage(tc.payload)
			got, err := ParseMessage(bytes.NewReader(frame))
			if err != nil {
				t.Fatalf("ParseMessage error: %v", err)
			}
			if !bytes.Equal(got, tc.payload) {
				t.Fatalf("payload mismatch: got %d bytes, want %d bytes", len(got), len(tc.payload))
			}
		})
	}
}

func TestParseMessageRejectsTooLarge(t *testing.T) {
	t.Parallel()

	// Write a header claiming maxFrameSize+1 bytes without a body.
	var hdr [4]byte
	hdr[0] = 0x01 // 0x01_00_00_01 = 16,777,217 > maxFrameSize
	hdr[1] = 0x00
	hdr[2] = 0x00
	hdr[3] = 0x01
	_, err := ParseMessage(bytes.NewReader(hdr[:]))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestParseMessageRejectsShortRead(t *testing.T) {
	t.Parallel()

	// Frame claims 10 bytes but provides only 2.
	frame := FrameMessage([]byte("hello world"))
	truncated := frame[:6] // 4-byte header + 2 payload bytes
	_, err := ParseMessage(bytes.NewReader(truncated))
	if err == nil {
		t.Fatal("expected error for truncated payload, got nil")
	}
}

func TestParseMessageRejectsEmptyReader(t *testing.T) {
	t.Parallel()

	_, err := ParseMessage(strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error on empty reader, got nil")
	}
}

func ExampleFrameMessage() {
	frame := FrameMessage([]byte("hello"))
	// The first 4 bytes hold the payload length as big-endian uint32.
	fmt.Printf("total bytes: %d\n", len(frame))
	fmt.Printf("payload: %s\n", string(frame[4:]))
	// Output:
	// total bytes: 9
	// payload: hello
}

func ExampleParseMessage() {
	frame := FrameMessage([]byte("world"))
	payload, err := ParseMessage(bytes.NewReader(frame))
	if err != nil {
		panic(err)
	}
	fmt.Println(string(payload))
	// Output:
	// world
}
```

The framing tests above are hermetic: they run on every `go test` with no network and no external module. The loopback integration tests below dial a real QUIC connection over the local UDP stack and need the `quic-go` module, so they live in a separate file behind a `//go:build online` tag. `go test` skips them by default; `go test -tags online` runs them.

Create `quictransport_online_test.go`:

```go
//go:build online

package quictransport

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"testing"
)

// ---- loopback integration tests (require quic-go module) ----

const testALPN = "quictransport/1"

func startTestServer(t *testing.T, handler Handler) (addr string, clientTLS *tls.Config, stop func()) {
	t.Helper()
	serverTLS, clientTLS, err := GenerateSelfSigned(testALPN)
	if err != nil {
		t.Fatalf("GenerateSelfSigned: %v", err)
	}
	srv, err := NewServer("127.0.0.1:0", serverTLS, handler)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	return srv.Addr().String(), clientTLS, func() {
		cancel()
		_ = srv.Close()
	}
}

func dialTestClient(t *testing.T, addr string, clientTLS *tls.Config) *Client {
	t.Helper()
	c, err := Dial(context.Background(), addr, clientTLS)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return c
}

func echoHandler(_ context.Context, req []byte) ([]byte, error) {
	resp := make([]byte, len(req))
	copy(resp, req)
	return resp, nil
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	addr, clientTLS, stop := startTestServer(t, echoHandler)
	defer stop()

	c := dialTestClient(t, addr, clientTLS)
	defer c.Close()

	resp, err := c.Request(context.Background(), []byte("ping"))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if string(resp) != "ping" {
		t.Fatalf("response = %q, want %q", resp, "ping")
	}
}

func TestMultiplexedStreams(t *testing.T) {
	t.Parallel()

	const n = 10
	addr, clientTLS, stop := startTestServer(t, echoHandler)
	defer stop()

	c := dialTestClient(t, addr, clientTLS)
	defer c.Close()

	// Open n streams concurrently over a single QUIC connection.
	type result struct {
		payload string
		err     error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := fmt.Sprintf("stream-%d", i)
			resp, err := c.Request(context.Background(), []byte(payload))
			results <- result{string(resp), err}
		}(i)
	}
	wg.Wait()
	close(results)

	for r := range results {
		if r.err != nil {
			t.Errorf("Request error: %v", r.err)
		}
	}
}

func TestStreamErrors(t *testing.T) {
	t.Parallel()

	// Handler that returns an error on odd-length payloads.
	handler := Handler(func(_ context.Context, req []byte) ([]byte, error) {
		if len(req)%2 != 0 {
			return nil, fmt.Errorf("odd payload length: %d", len(req))
		}
		return req, nil
	})
	addr, clientTLS, stop := startTestServer(t, handler)
	defer stop()

	c := dialTestClient(t, addr, clientTLS)
	defer c.Close()

	// Even-length payload: succeeds.
	resp, err := c.Request(context.Background(), []byte("ok"))
	if err != nil {
		t.Fatalf("even payload: %v", err)
	}
	if string(resp) != "ok" {
		t.Fatalf("even payload response = %q, want %q", resp, "ok")
	}

	// Odd-length payload: handler errors, stream is reset.
	// The connection itself must remain open for subsequent requests.
	_, err = c.Request(context.Background(), []byte("bad"))
	if err == nil {
		t.Fatal("expected error for odd payload, got nil")
	}

	// The connection is still alive: subsequent even-length request succeeds.
	resp, err = c.Request(context.Background(), []byte("ok"))
	if err != nil {
		t.Fatalf("post-error even payload: %v", err)
	}
	if string(resp) != "ok" {
		t.Fatalf("post-error response = %q, want %q", resp, "ok")
	}
}

// Your turn: add TestLargePayload that sends a 1 MiB payload and verifies
// the echo response has the same length.
//
// func TestLargePayload(t *testing.T) { ... }
```

**Note**: `TestMultiplexedStreams` uses `range n` (Go 1.22+ range-over-integer). If your `go.mod` pins an earlier version, replace `for i := range n` with `for i := 0; i < n; i++`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"example.com/quictransport"
)

func main() {
	const alpn = "quictransport/1"

	serverTLS, clientTLS, err := quictransport.GenerateSelfSigned(alpn)
	if err != nil {
		log.Fatalf("generate TLS: %v", err)
	}

	// Start an echo server on an OS-assigned port.
	handler := quictransport.Handler(func(ctx context.Context, req []byte) ([]byte, error) {
		return req, nil
	})
	srv, err := quictransport.NewServer("127.0.0.1:0", serverTLS, handler)
	if err != nil {
		log.Fatalf("NewServer: %v", err)
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	go func() { _ = srv.Serve(serverCtx) }()

	fmt.Printf("server listening on %s\n", srv.Addr())

	// Dial a single connection and send 10 requests concurrently over 10 streams.
	c, err := quictransport.Dial(context.Background(), srv.Addr().String(), clientTLS)
	if err != nil {
		log.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	const concurrency = 10
	start := time.Now()
	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := fmt.Sprintf("request-%d", i)
			resp, err := c.Request(context.Background(), []byte(payload))
			if err != nil {
				log.Printf("stream %d: %v", i, err)
				return
			}
			fmt.Printf("stream %d: sent %q received %q\n", i, payload, string(resp))
		}(i)
	}
	wg.Wait()
	fmt.Printf("%d concurrent streams over 1 QUIC connection in %v\n", concurrency, time.Since(start).Round(time.Millisecond))
}
```

Run it:

```bash
cd ~/go-exercises/quictransport
go run ./cmd/demo
```

## Common Mistakes

### Wrong ALPN or Missing NextProtos

Wrong: creating a QUIC server with no `NextProtos` set in the TLS config. quic-go requires ALPN; if the server and client advertise no common protocol, the TLS handshake fails with a `tls: no application protocol` alert.

Fix: always set `tlsConf.NextProtos = []string{"your-protocol/version"}` on both the server and the client before passing the config to quic-go.

### Treating stream.Close() as Connection Close

Wrong: calling `stream.Close()` and expecting the connection to end. `Close()` closes only the write half of that stream; the connection and all other streams remain open.

Fix: close the connection with `conn.CloseWithError(code, reason)`. Use `stream.Close()` only to signal end-of-data on a single stream's send side.

### Reading After CancelRead

Wrong: calling `stream.CancelRead(1)` and then attempting to read from the same stream. `CancelRead` discards buffered data and tells the peer to stop sending; any subsequent `Read` returns an error.

Fix: cancel either the read half or the write half depending on which operation failed, then return without touching the other half or re-using the stream.

### 0-RTT Without a Session Cache

Wrong: calling `quic.DialAddrEarly` without setting `tls.Config.ClientSessionCache`. Without a cached session ticket there is no 0-RTT keying material; the connection falls back to 1-RTT silently.

Fix: set `ClientSessionCache: tls.NewLRUClientSessionCache(100)` on the client TLS config and reuse the same `tls.Config` across `Dial` calls. Check `conn.ConnectionState().Used0RTT` to confirm 0-RTT was used.

### Blocking AcceptStream Without a Context Deadline

Wrong: calling `conn.AcceptStream(context.Background())` in a loop that never checks whether the connection has been closed. If the peer disappears without a clean FIN, `AcceptStream` blocks until the idle timeout fires.

Fix: pass a context with a deadline derived from the parent cancellation. In `serveConn`, pass `ctx` from `Serve` rather than `context.Background()`, so that a cancelled `Serve` context also unblocks `AcceptStream`.

## Verification

From `~/go-exercises/quictransport`:

```bash
# Framing code (no external deps): check format and vet directly.
gofmt -l frame.go tls.go
go vet ./...          # requires quic-go module; run after go get

# Full test suite (requires quic-go downloaded):
go test -count=1 -race ./...

# Build the demo:
go build ./cmd/demo

# Run the demo:
go run ./cmd/demo
```

The framing tests (`TestFrameMessageRoundTrip`, `TestParseMessage*`, `ExampleFrameMessage`, `ExampleParseMessage`) run without network. The integration tests (`TestRoundTrip`, `TestMultiplexedStreams`, `TestStreamErrors`) use loopback UDP; they require the `quic-go` module to be downloaded but do not require external internet connectivity during the test run.

Add at least one test of your own: `TestLargePayload` that sends a 1 MiB payload and verifies the echo has the same length. This exercises the multi-packet path through quic-go's flow control.

## Summary

- QUIC solves TCP head-of-line blocking by giving each stream independent loss recovery; a lost packet on stream N does not block reads on stream M.
- QUIC integrates TLS 1.3 into the transport handshake: 1 RTT for first connections, 0 RTT for repeat connections when the client holds a valid session ticket and the server enables `Allow0RTT`.
- Every QUIC connection requires TLS 1.3 and ALPN; missing `NextProtos` on either side causes the handshake to fail.
- `stream.Close()` closes the write half only; the read half remains open. `conn.CloseWithError` closes the connection.
- A single QUIC connection multiplexes thousands of independent streams; connection pooling is unnecessary.
- `CancelWrite(code)` and `CancelRead(code)` abort individual stream halves with application-defined error codes without affecting the connection or other streams.

## What's Next

Next: [HTTP/3 over QUIC](../25-http3-over-quic/25-http3-over-quic.md).

## Resources

- [quic-go package documentation](https://pkg.go.dev/github.com/quic-go/quic-go) — authoritative API reference for all types and functions used in this lesson
- [quic-go.net: Running a QUIC Server](https://quic-go.net/docs/quic/server/) — official server guide with Accept and stream patterns
- [quic-go.net: QUIC Streams](https://quic-go.net/docs/quic/streams/) — stream lifecycle, CancelRead/CancelWrite, unidirectional streams
- [RFC 9000 — QUIC: A UDP-Based Multiplexed and Secure Transport](https://datatracker.ietf.org/doc/html/rfc9000) — normative specification for connection IDs, stream framing, and flow control
- [RFC 9001 — Using TLS to Secure QUIC](https://datatracker.ietf.org/doc/html/rfc9001) — specifies the mandatory TLS 1.3 and ALPN requirements
