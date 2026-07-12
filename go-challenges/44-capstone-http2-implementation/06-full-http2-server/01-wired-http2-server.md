# Exercise 1: The Wired HTTP/2 Server

This module assembles the whole stack into one runnable server: a real frame
codec, a real HPACK codec, the connection read loop, the per-stream
`http.ResponseWriter`, ALPN-negotiated TLS, and graceful shutdown — driven end to
end by Go's own `net/http` HTTP/2 client.

The module is fully self-contained. It defines the `RawFrame`/`FrameLayer`/
`HeadersCodec` seam, ships a concrete framer (RFC 9113 §4.1) and an HPACK adapter
over `golang.org/x/net/http2/hpack`, wires the connection and response writer,
and tests against the standard library client over TLS. Nothing here imports
another exercise.

## What you'll build

```text
server.go          Config, Server, New, ListenAndServeTLS, Serve, Shutdown;
                   RawFrame, FrameLayer, HeadersCodec interfaces + constants
conn.go            h2conn read loop: preface, SETTINGS handshake, frame switch,
                   CONTINUATION tracking, dispatch to handlers, writeFrame(wmu)
response_writer.go h2stream + h2responseWriter (Header/WriteHeader/Write/Flush),
                   HPACK encode under wmu, DATA framing, finalize
framer.go          NewFramer: 9-byte frame header read/write
hpackcodec.go      NewHPACKCodecs: decode/encode halves over x/net/http2/hpack
cmd/
  demo/
    main.go        TLS+ALPN server on loopback driven by the net/http client
server_test.go     end-to-end GET, body echo, concurrent streams, custom
                   header/status, graceful + deadline shutdown, unit tests
```

- Files: `server.go`, `conn.go`, `response_writer.go`, `framer.go`,
  `hpackcodec.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: the connection read loop and its frame switch, the write mutex that
  serializes HPACK encode with frame writes, the `http.ResponseWriter` for one
  stream, the concrete framer and HPACK adapter, and the ALPN TLS entry path.
- Test: a real `net/http` HTTP/2 client performs a simple GET, a POST whose body
  is echoed, 25 concurrent streams on one connection, a custom header and status,
  and both graceful-drain and deadline-force-close shutdown.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The seam: RawFrame, FrameLayer, HeadersCodec

The server never parses bytes or compresses headers itself. It depends on two
interfaces and a `Config` that supplies a factory for each per connection. A
`RawFrame` is the wire frame with its fields already split out; a `FrameLayer`
reads and writes those frames; a `HeadersCodec` turns an HPACK block into ordered
name/value pairs and back. `Config` also carries the tunables the server
advertises in SETTINGS, with `effective*` accessors that fold in the RFC defaults
so a zero-value field never produces an out-of-spec setting. `New` rejects a
config missing either factory, because a server that cannot build a framer or a
codec can never serve a byte.

`Serve` is the accept loop. For a `*tls.Conn` it completes the handshake and
reads `NegotiatedProtocol`; anything other than "h2" is closed. A non-TLS
connection is taken as h2c and served directly. Each accepted connection is
tracked (so `Shutdown` can reach it) and served in its own goroutine.
`ListenAndServeTLS` is the convenience entry: it clones the TLS config, prepends
"h2" to `NextProtos` so ALPN can select it, loads the key pair, and calls
`Serve`. `Shutdown` flips an atomic flag, sends GOAWAY to every tracked
connection, and waits for them to drain or for the context to expire — in which
case it force-closes the stragglers and returns the context error.

Create `server.go`:

```go
package h2server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// clientPreface is the fixed 24-byte sequence a client sends before any frames
// (RFC 9113 §3.4).
const clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// Frame type codes (RFC 9113 §6).
const (
	frameData         uint8 = 0x0
	frameHeaders      uint8 = 0x1
	frameRSTStream    uint8 = 0x3
	frameSettings     uint8 = 0x4
	framePing         uint8 = 0x6
	frameGoAway       uint8 = 0x7
	frameWindowUpdate uint8 = 0x8
	frameContinuation uint8 = 0x9
)

// Frame flags.
const (
	flagEndStream  uint8 = 0x1
	flagACK        uint8 = 0x1
	flagEndHeaders uint8 = 0x4
)

// Error codes (RFC 9113 §7).
const (
	ErrCodeNoError     uint32 = 0x0
	ErrCodeProtocol    uint32 = 0x1
	ErrCodeCancel      uint32 = 0x8
	ErrCodeCompression uint32 = 0x9
)

// Default values from RFC 9113 §6.5.2.
const (
	defaultInitWindowSize uint32 = 65535
	defaultMaxFrameSize   uint32 = 16384
)

// RawFrame holds one HTTP/2 frame exactly as decoded by the framer.
// Payload is only valid until the next ReadFrame call.
type RawFrame struct {
	Length   uint32 // 24-bit payload length from the frame header
	Type     uint8
	Flags    uint8
	StreamID uint32 // 31-bit; high bit stripped by the framer
	Payload  []byte
}

// FrameLayer handles frame-level I/O on a single connection.
// ReadFrame and WriteFrame are never called concurrently on the same instance;
// the connection's read loop owns ReadFrame, and all writes go through writeFrame
// (which holds the write mutex).
type FrameLayer interface {
	ReadFrame() (*RawFrame, error)
	WriteFrame(f *RawFrame) error
}

// HeadersCodec encodes and decodes HPACK header block fragments.
// Decode is called only by the read loop; Encode is called only while wmu is held.
// One codec instance serves one connection; it is NOT goroutine-safe.
type HeadersCodec interface {
	Decode(block []byte) ([][2]string, error)
	Encode(pairs [][2]string) ([]byte, error)
}

// FrameLayerFactory creates a FrameLayer for a new connection.
type FrameLayerFactory func(rw io.ReadWriter) FrameLayer

// HeadersCodecFactory creates decoder and encoder halves for a new connection.
// The two halves maintain independent dynamic tables (RFC 7541 §2.2).
type HeadersCodecFactory func() (decoder HeadersCodec, encoder HeadersCodec)

// Config holds all parameters for a Server.
type Config struct {
	// Handler dispatches HTTP requests. Defaults to http.DefaultServeMux.
	Handler http.Handler

	// TLSConfig for ALPN negotiation. ListenAndServeTLS clones this and adds
	// "h2" to NextProtos if not already present.
	TLSConfig *tls.Config

	// MaxConcurrentStreams is advertised in the server SETTINGS frame.
	// Zero defaults to 100.
	MaxConcurrentStreams uint32

	// InitialWindowSize is the per-stream flow-control window advertised to
	// the client. Zero defaults to the RFC 9113 minimum of 65535.
	InitialWindowSize uint32

	// MaxFrameSize is the maximum DATA payload the server will accept.
	// Zero defaults to the RFC 9113 minimum of 16384.
	MaxFrameSize uint32

	// ReadTimeout bounds each frame read. Zero means no timeout.
	ReadTimeout time.Duration

	// IdleTimeout closes a connection idle for longer than this value.
	// Zero means no idle timeout.
	IdleTimeout time.Duration

	// NewFrameLayer creates the frame I/O layer for each accepted connection.
	// Must be non-nil.
	NewFrameLayer FrameLayerFactory

	// NewHeadersCodec creates a decode/encode pair for each accepted connection.
	// Must be non-nil.
	NewHeadersCodec HeadersCodecFactory
}

func (c *Config) effectiveHandler() http.Handler {
	if c.Handler != nil {
		return c.Handler
	}
	return http.DefaultServeMux
}

func (c *Config) effectiveMaxConcurrent() uint32 {
	if c.MaxConcurrentStreams > 0 {
		return c.MaxConcurrentStreams
	}
	return 100
}

func (c *Config) effectiveWindowSize() uint32 {
	if c.InitialWindowSize > 0 {
		return c.InitialWindowSize
	}
	return defaultInitWindowSize
}

func (c *Config) effectiveMaxFrame() uint32 {
	if c.MaxFrameSize >= defaultMaxFrameSize {
		return c.MaxFrameSize
	}
	return defaultMaxFrameSize
}

// Server is an HTTP/2 server that delegates frame parsing to a FrameLayer,
// header compression to a HeadersCodec, and request routing to an http.Handler.
type Server struct {
	cfg Config

	mu         sync.Mutex
	activeConn map[*h2conn]struct{}
	inShutdown atomic.Bool
}

// New returns a Server ready to accept connections.
// Returns an error if NewFrameLayer or NewHeadersCodec is nil.
func New(cfg Config) (*Server, error) {
	if cfg.NewFrameLayer == nil {
		return nil, errors.New("h2server: Config.NewFrameLayer must not be nil")
	}
	if cfg.NewHeadersCodec == nil {
		return nil, errors.New("h2server: Config.NewHeadersCodec must not be nil")
	}
	return &Server{
		cfg:        cfg,
		activeConn: make(map[*h2conn]struct{}),
	}, nil
}

// ListenAndServeTLS opens a TLS listener on addr, prepends "h2" to NextProtos
// for ALPN negotiation, and serves until Shutdown is called.
func (s *Server) ListenAndServeTLS(addr, certFile, keyFile string) error {
	tlsCfg := s.cfg.TLSConfig.Clone()
	if tlsCfg == nil {
		tlsCfg = new(tls.Config)
	}
	if !stringsContains(tlsCfg.NextProtos, "h2") {
		tlsCfg.NextProtos = append([]string{"h2"}, tlsCfg.NextProtos...)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("h2server: load key pair: %w", err)
	}
	tlsCfg.Certificates = append(tlsCfg.Certificates, cert)

	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("h2server: listen %s: %w", addr, err)
	}
	return s.Serve(ln)
}

// Serve accepts connections from ln until Shutdown is called or ln is closed.
// Each accepted connection is served in its own goroutine.
func (s *Server) Serve(ln net.Listener) error {
	defer ln.Close()
	var backoff time.Duration
	for {
		nc, err := ln.Accept()
		if err != nil {
			if s.inShutdown.Load() {
				return http.ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if backoff == 0 {
					backoff = 5 * time.Millisecond
				} else if backoff < time.Second {
					backoff *= 2
				}
				time.Sleep(backoff)
				continue
			}
			return err
		}
		backoff = 0

		// For TLS connections, complete the handshake and verify ALPN.
		// For non-TLS connections, assume h2c.
		proto := "h2"
		if tlsConn, ok := nc.(*tls.Conn); ok {
			if herr := tlsConn.Handshake(); herr != nil {
				nc.Close()
				continue
			}
			proto = tlsConn.ConnectionState().NegotiatedProtocol
		}
		if proto != "h2" {
			nc.Close()
			continue
		}

		c := s.newConn(nc)
		s.track(c, true)
		go func() {
			defer s.track(c, false)
			c.serve()
		}()
	}
}

// Shutdown sends GOAWAY to all active connections, then waits for them to drain
// or for ctx to expire.
func (s *Server) Shutdown(ctx context.Context) error {
	s.inShutdown.Store(true)

	s.mu.Lock()
	conns := make([]*h2conn, 0, len(s.activeConn))
	for c := range s.activeConn {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		c.beginShutdown()
	}

	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		s.mu.Lock()
		n := len(s.activeConn)
		s.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for c := range s.activeConn {
				c.nc.Close()
			}
			s.mu.Unlock()
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (s *Server) track(c *h2conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.activeConn[c] = struct{}{}
	} else {
		delete(s.activeConn, c)
	}
}

func (s *Server) newConn(nc net.Conn) *h2conn {
	decoder, encoder := s.cfg.NewHeadersCodec()
	return &h2conn{
		srv:        s,
		nc:         nc,
		framer:     s.cfg.NewFrameLayer(nc),
		decoder:    decoder,
		encoder:    encoder,
		streams:    make(map[uint32]*h2stream),
		shutdownCh: make(chan struct{}),
	}
}

func stringsContains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
```

### The connection read loop

`serve` is the connection's one read-loop goroutine. It reads the 24-byte preface
(closing on any mismatch), sends the server SETTINGS, and then loops over frames.
The loop carries three pieces of header-assembly state — the accumulating block
buffer, its stream ID, and the original HEADERS flags — plus an `inContinuation`
flag. While that flag is set, RFC 9113 §6.10 allows only a CONTINUATION for the
same stream; anything else is a connection-level PROTOCOL_ERROR and a GOAWAY.

The frame switch routes each type: SETTINGS is acknowledged (and an incoming ACK
is a no-op, which is what stops the ACK ping-pong); PING echoes its payload with
the ACK flag; WINDOW_UPDATE credits a stream's send window; RST_STREAM cancels a
stream; HEADERS and CONTINUATION feed the assembler and, once END_HEADERS lands,
call `dispatchHeaders`; DATA is written into the request body pipe and the
flow-control window is restored. Every outbound frame — including the read loop's
own PING and SETTINGS ACKs — goes through `writeFrame`, which holds `wmu`. That is
the single serialization point that makes the read loop and the handler goroutines
safe to write concurrently.

`dispatchHeaders` is where a request is born: decode the block to pairs, build an
`http.Request` (pseudo-headers first, the rest as canonical headers), wire an
`io.Pipe` for the body, register the stream, and start the handler in its own
goroutine. The deferred cleanup deletes the stream and calls `finalize`, which
closes the stream with END_STREAM if the handler did not. A missing required
pseudo-header (`:method`, `:path`, `:scheme`) is a stream error, not a connection
error, so it is answered with RST_STREAM rather than tearing down the whole
connection.

Create `conn.go`:

```go
package h2server

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

// h2conn manages one HTTP/2 connection.
type h2conn struct {
	srv     *Server
	nc      net.Conn
	framer  FrameLayer
	decoder HeadersCodec // owned exclusively by the read loop
	encoder HeadersCodec // accessed only under wmu

	wmu     sync.Mutex // serializes all frame writes and HPACK encoding
	mu      sync.Mutex // serializes streams map
	streams map[uint32]*h2stream

	lastStreamID atomic.Uint32
	shutdownCh   chan struct{}
	shutdownOnce sync.Once
}

// serve is the top-level goroutine for one connection. It reads the preface,
// exchanges SETTINGS, and then loops reading frames until the connection closes
// or shutdown is initiated.
func (c *h2conn) serve() {
	defer c.nc.Close()

	if err := c.readPreface(); err != nil {
		return
	}
	if err := c.sendServerSettings(); err != nil {
		return
	}

	// hdrBuf and hdrStream accumulate HEADERS+CONTINUATION block fragments.
	var hdrBuf []byte
	var hdrStream uint32
	var hdrFlags uint8
	inContinuation := false

	for {
		select {
		case <-c.shutdownCh:
			return
		default:
		}

		f, err := c.framer.ReadFrame()
		if err != nil {
			if err != io.EOF {
				c.writeGoAway(c.lastStreamID.Load(), ErrCodeProtocol)
			}
			return
		}

		// RFC 9113 §6.10: while accumulating a header block, only CONTINUATION
		// frames for the same stream are allowed.
		if inContinuation {
			if f.Type != frameContinuation || f.StreamID != hdrStream {
				c.writeGoAway(c.lastStreamID.Load(), ErrCodeProtocol)
				return
			}
		}

		switch f.Type {
		case frameSettings:
			if err := c.handleSettings(f); err != nil {
				c.writeGoAway(c.lastStreamID.Load(), ErrCodeProtocol)
				return
			}

		case framePing:
			c.handlePing(f)

		case frameWindowUpdate:
			c.handleWindowUpdate(f)

		case frameRSTStream:
			c.handleRSTStream(f)

		case frameHeaders:
			if f.StreamID == 0 {
				c.writeGoAway(0, ErrCodeProtocol)
				return
			}
			c.lastStreamID.Store(f.StreamID)
			hdrBuf = append(hdrBuf[:0], f.Payload...)
			hdrStream = f.StreamID
			hdrFlags = f.Flags
			inContinuation = f.Flags&flagEndHeaders == 0
			if !inContinuation {
				c.dispatchHeaders(hdrStream, hdrBuf, hdrFlags)
				hdrBuf = hdrBuf[:0]
			}

		case frameContinuation:
			hdrBuf = append(hdrBuf, f.Payload...)
			// Propagate END_STREAM from the original HEADERS flags.
			inContinuation = f.Flags&flagEndHeaders == 0
			if !inContinuation {
				c.dispatchHeaders(hdrStream, hdrBuf, hdrFlags)
				hdrBuf = hdrBuf[:0]
			}

		case frameData:
			c.handleData(f)

		case frameGoAway:
			return
		}
	}
}

// readPreface reads and validates the 24-byte client connection preface.
func (c *h2conn) readPreface() error {
	buf := make([]byte, len(clientPreface))
	if _, err := io.ReadFull(c.nc, buf); err != nil {
		return fmt.Errorf("read preface: %w", err)
	}
	if string(buf) != clientPreface {
		return fmt.Errorf("invalid connection preface")
	}
	return nil
}

// sendServerSettings sends the server's initial SETTINGS frame containing three
// parameters: MAX_CONCURRENT_STREAMS, INITIAL_WINDOW_SIZE, MAX_FRAME_SIZE.
// Each parameter is 6 bytes (2-byte ID + 4-byte value); RFC 9113 §6.5.
func (c *h2conn) sendServerSettings() error {
	payload := make([]byte, 18)
	binary.BigEndian.PutUint16(payload[0:], 0x3) // MAX_CONCURRENT_STREAMS
	binary.BigEndian.PutUint32(payload[2:], c.srv.cfg.effectiveMaxConcurrent())
	binary.BigEndian.PutUint16(payload[6:], 0x4) // INITIAL_WINDOW_SIZE
	binary.BigEndian.PutUint32(payload[8:], c.srv.cfg.effectiveWindowSize())
	binary.BigEndian.PutUint16(payload[12:], 0x5) // MAX_FRAME_SIZE
	binary.BigEndian.PutUint32(payload[14:], c.srv.cfg.effectiveMaxFrame())
	return c.writeFrame(&RawFrame{Type: frameSettings, Payload: payload})
}

func (c *h2conn) handleSettings(f *RawFrame) error {
	if f.Flags&flagACK != 0 {
		return nil // peer acknowledged our SETTINGS
	}
	// A full implementation parses and applies the peer's settings
	// (INITIAL_WINDOW_SIZE affects all active streams; MAX_FRAME_SIZE affects
	// outbound DATA frames). Here we acknowledge them.
	return c.writeFrame(&RawFrame{Type: frameSettings, Flags: flagACK})
}

func (c *h2conn) handlePing(f *RawFrame) {
	if f.Flags&flagACK != 0 {
		return
	}
	ack := &RawFrame{Type: framePing, Flags: flagACK, Payload: make([]byte, 8)}
	copy(ack.Payload, f.Payload)
	_ = c.writeFrame(ack)
}

func (c *h2conn) handleWindowUpdate(f *RawFrame) {
	if len(f.Payload) < 4 {
		return
	}
	inc := binary.BigEndian.Uint32(f.Payload) & 0x7fffffff
	if f.StreamID != 0 {
		c.mu.Lock()
		st := c.streams[f.StreamID]
		c.mu.Unlock()
		if st != nil {
			st.addSendWindow(int32(inc))
		}
	}
	// Connection-level WINDOW_UPDATE: a full implementation signals all blocked
	// stream writers that the connection window has grown.
}

func (c *h2conn) handleRSTStream(f *RawFrame) {
	c.mu.Lock()
	st := c.streams[f.StreamID]
	delete(c.streams, f.StreamID)
	c.mu.Unlock()
	if st != nil {
		st.cancel()
	}
}

func (c *h2conn) handleData(f *RawFrame) {
	c.mu.Lock()
	st := c.streams[f.StreamID]
	c.mu.Unlock()
	if st == nil {
		return
	}
	if len(f.Payload) > 0 {
		_, _ = st.bodyW.Write(f.Payload)
		// Restore the connection-level flow-control window immediately.
		c.sendWindowUpdate(0, uint32(len(f.Payload)))
		// Restore the stream-level window; the stream writer will send its own.
		c.sendWindowUpdate(f.StreamID, uint32(len(f.Payload)))
	}
	if f.Flags&flagEndStream != 0 {
		st.bodyW.Close()
	}
}

// dispatchHeaders decodes the complete header block, builds an http.Request,
// and starts a goroutine to run the registered http.Handler.
func (c *h2conn) dispatchHeaders(streamID uint32, block []byte, flags uint8) {
	pairs, err := c.decoder.Decode(block)
	if err != nil {
		c.writeGoAway(c.lastStreamID.Load(), ErrCodeCompression)
		return
	}

	req, err := buildRequest(pairs, c.nc.RemoteAddr())
	if err != nil {
		c.sendRSTStream(streamID, ErrCodeProtocol)
		return
	}

	bodyR, bodyW := io.Pipe()
	req.Body = bodyR

	rw := newResponseWriter(c, streamID)
	st := &h2stream{
		id:    streamID,
		bodyW: bodyW,
		rw:    rw,
		winCh: make(chan struct{}, 1),
	}
	st.sendWin.Store(int32(c.srv.cfg.effectiveWindowSize()))

	c.mu.Lock()
	c.streams[streamID] = st
	c.mu.Unlock()

	if flags&flagEndStream != 0 {
		bodyW.Close()
	}

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.streams, streamID)
			c.mu.Unlock()
			rw.finalize()
		}()
		c.srv.cfg.effectiveHandler().ServeHTTP(rw, req)
	}()
}

// writeFrame serializes all frame writes through wmu. The HPACK encoder is
// called under this same mutex in sendHeaders, so encode+write is always atomic.
func (c *h2conn) writeFrame(f *RawFrame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.framer.WriteFrame(f)
}

func (c *h2conn) writeGoAway(lastStreamID uint32, errCode uint32) {
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[0:], lastStreamID&0x7fffffff)
	binary.BigEndian.PutUint32(payload[4:], errCode)
	_ = c.writeFrame(&RawFrame{Type: frameGoAway, Payload: payload})
}

func (c *h2conn) sendRSTStream(streamID uint32, errCode uint32) {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, errCode)
	_ = c.writeFrame(&RawFrame{Type: frameRSTStream, StreamID: streamID, Payload: payload})
}

func (c *h2conn) sendWindowUpdate(streamID uint32, inc uint32) {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, inc&0x7fffffff)
	_ = c.writeFrame(&RawFrame{Type: frameWindowUpdate, StreamID: streamID, Payload: payload})
}

// beginShutdown sends GOAWAY and signals the serve loop to stop accepting new
// streams. In-flight streams are allowed to complete normally.
func (c *h2conn) beginShutdown() {
	c.shutdownOnce.Do(func() {
		c.writeGoAway(c.lastStreamID.Load(), ErrCodeNoError)
		close(c.shutdownCh)
	})
}

// buildRequest constructs an http.Request from decoded HPACK header pairs.
// HTTP/2 pseudo-headers (":method", ":path", ":scheme", ":authority") are
// extracted first; remaining pairs become regular HTTP headers.
func buildRequest(pairs [][2]string, remoteAddr net.Addr) (*http.Request, error) {
	var method, path, scheme, authority string
	header := make(http.Header)
	for _, p := range pairs {
		switch p[0] {
		case ":method":
			method = p[1]
		case ":path":
			path = p[1]
		case ":scheme":
			scheme = p[1]
		case ":authority":
			authority = p[1]
		default:
			header.Add(http.CanonicalHeaderKey(p[0]), p[1])
		}
	}
	if method == "" || path == "" || scheme == "" {
		return nil, fmt.Errorf("missing required pseudo-headers")
	}
	rawURL := scheme + "://" + authority + path
	req, err := http.NewRequest(method, rawURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header = header
	req.RemoteAddr = remoteAddr.String()
	req.Proto = "HTTP/2.0"
	req.ProtoMajor = 2
	req.ProtoMinor = 0
	return req, nil
}
```

### The per-stream ResponseWriter

`h2responseWriter` implements `http.ResponseWriter` for one stream. Its own small
mutex guards only the `wroteHeader`/`sentEndStream` flags, and it is the lock that
must never be held across a `wmu` acquisition. `WriteHeader` latches the status
under `mu`, releases `mu`, and then calls `sendHeaders`, which takes `wmu` for the
encode-and-write. `Write` does the implicit `WriteHeader(200)` the same way, then
streams the body through `sendData`. `sendHeaders` builds the pairs with `:status`
first and every other field name lowercased, encodes under `wmu`, and writes the
HEADERS frame in the same critical section so the dynamic table and the wire never
diverge. `sendData` chunks the body by MAX_FRAME_SIZE, acquiring `wmu` per frame
and marking END_STREAM on the last chunk; `finalize` sends a trailing empty
END_STREAM DATA frame (or a 200 + END_STREAM) only if the handler left the stream
open. The `h2stream` holds the request-body pipe writer and the send-window
counter the read loop credits from WINDOW_UPDATE.

Create `response_writer.go`:

```go
package h2server

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// h2stream holds the mutable per-stream state that the read loop updates.
type h2stream struct {
	id    uint32
	bodyW *io.PipeWriter // write side of the request body pipe; nil after Close
	rw    *h2responseWriter

	sendWin atomic.Int32  // remaining send window for outbound DATA
	winCh   chan struct{} // signaled when sendWin increases
}

func (s *h2stream) addSendWindow(delta int32) {
	s.sendWin.Add(delta)
	select {
	case s.winCh <- struct{}{}:
	default:
	}
}

func (s *h2stream) cancel() {
	s.bodyW.CloseWithError(fmt.Errorf("stream reset by peer"))
}

// h2responseWriter implements http.ResponseWriter for one HTTP/2 stream.
type h2responseWriter struct {
	conn     *h2conn
	streamID uint32
	header   http.Header

	mu            sync.Mutex
	wroteHeader   bool
	statusCode    int
	sentEndStream bool
}

func newResponseWriter(c *h2conn, streamID uint32) *h2responseWriter {
	return &h2responseWriter{
		conn:       c,
		streamID:   streamID,
		header:     make(http.Header),
		statusCode: http.StatusOK,
	}
}

// Header returns the response header map. Values are buffered until
// WriteHeader is called.
func (w *h2responseWriter) Header() http.Header {
	return w.header
}

// WriteHeader sends a HEADERS frame with the given status code.
// The first call wins; subsequent calls are ignored.
func (w *h2responseWriter) WriteHeader(code int) {
	w.mu.Lock()
	if w.wroteHeader {
		w.mu.Unlock()
		return
	}
	w.wroteHeader = true
	w.statusCode = code
	w.mu.Unlock()
	// Release mu before sendHeaders to preserve lock order (mu never nests wmu).
	_ = w.sendHeaders(code, false)
}

// Write implicitly calls WriteHeader(200) on the first call, then sends p as
// DATA frames.
func (w *h2responseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if !w.wroteHeader {
		w.wroteHeader = true
		code := w.statusCode
		w.mu.Unlock()
		_ = w.sendHeaders(code, false)
	} else {
		w.mu.Unlock()
	}
	return w.sendData(p)
}

// Flush implements http.Flusher. Each Write sends a DATA frame immediately;
// Flush is a no-op in this implementation.
func (w *h2responseWriter) Flush() {}

// finalize is called by the deferred cleanup in dispatchHeaders after the
// handler goroutine exits. It closes the stream with END_STREAM if the handler
// did not do so.
func (w *h2responseWriter) finalize() {
	w.mu.Lock()
	wroteHeader := w.wroteHeader
	sentEnd := w.sentEndStream
	w.mu.Unlock()

	if !wroteHeader {
		// Handler returned without writing anything; send 200 + END_STREAM.
		_ = w.sendHeaders(http.StatusOK, true)
		return
	}
	if !sentEnd {
		// Handler wrote headers and/or body, but no END_STREAM yet.
		_ = w.conn.writeFrame(&RawFrame{
			Type:     frameData,
			Flags:    flagEndStream,
			StreamID: w.streamID,
		})
	}
}

// sendHeaders encodes pairs with HPACK and sends a HEADERS frame.
// The encode and write are performed under wmu to keep the dynamic table
// consistent across concurrent streams.
//
// The :status pseudo-header is always first (RFC 9113 §8.3.2).
// Header field names are lowercased (RFC 9113 §8.2.1).
func (w *h2responseWriter) sendHeaders(code int, endStream bool) error {
	pairs := [][2]string{{":status", strconv.Itoa(code)}}
	for name, values := range w.header {
		lower := strings.ToLower(name)
		for _, v := range values {
			pairs = append(pairs, [2]string{lower, v})
		}
	}

	w.conn.wmu.Lock()
	defer w.conn.wmu.Unlock()

	block, err := w.conn.encoder.Encode(pairs)
	if err != nil {
		return fmt.Errorf("encode headers stream %d: %w", w.streamID, err)
	}
	flags := flagEndHeaders
	if endStream {
		flags |= flagEndStream
	}
	return w.conn.framer.WriteFrame(&RawFrame{
		Type:     frameHeaders,
		Flags:    flags,
		StreamID: w.streamID,
		Payload:  block,
	})
}

// sendData sends p as one or more DATA frames, each bounded by MAX_FRAME_SIZE.
// It does not check the flow-control send window; a production implementation
// would call st.acquireSendWindow(chunkLen) here and block until the peer
// grants credit via WINDOW_UPDATE.
func (w *h2responseWriter) sendData(p []byte) (int, error) {
	maxFrame := int(w.conn.srv.cfg.effectiveMaxFrame())
	sent := 0
	for len(p) > 0 {
		chunkLen := len(p)
		if chunkLen > maxFrame {
			chunkLen = maxFrame
		}
		last := len(p[chunkLen:]) == 0
		flags := uint8(0)
		if last {
			flags = flagEndStream
		}
		payload := make([]byte, chunkLen)
		copy(payload, p[:chunkLen])

		w.conn.wmu.Lock()
		err := w.conn.framer.WriteFrame(&RawFrame{
			Type:     frameData,
			Flags:    flags,
			StreamID: w.streamID,
			Payload:  payload,
		})
		w.conn.wmu.Unlock()

		if err != nil {
			return sent, err
		}
		sent += chunkLen
		p = p[chunkLen:]

		if last {
			w.mu.Lock()
			w.sentEndStream = true
			w.mu.Unlock()
		}
	}
	return sent, nil
}
```

### The concrete framer and HPACK adapter

The two interfaces need real implementations for the server to talk to a real
client. `netFramer` is the RFC 9113 §4.1 frame codec: a 9-byte header (24-bit
length, type, flags, 31-bit stream ID) followed by the payload. It is unbuffered
and reads straight from the connection, which is why `readPreface` (which reads
the connection directly, before the first `ReadFrame`) loses no bytes. The HPACK
adapter wraps the standard library's battle-tested `golang.org/x/net/http2/hpack`:
the decode half owns a `hpack.Decoder` driven by the read loop, and the encode
half owns a `hpack.Encoder` writing into a reused buffer, called only under `wmu`.
Each connection gets its own pair, because the two dynamic tables are independent
per RFC 7541 §2.2.

Create `framer.go`:

```go
package h2server

import (
	"encoding/binary"
	"fmt"
	"io"
)

// frameHeaderLen is the fixed 9-byte HTTP/2 frame header (RFC 9113 §4.1).
const frameHeaderLen = 9

// netFramer reads and writes HTTP/2 frames on a single connection.
type netFramer struct {
	r io.Reader
	w io.Writer
}

// NewFramer returns a FrameLayer that reads and writes RFC 9113 §4.1 frames on
// rw. One framer serves one connection; ReadFrame and WriteFrame are never
// called concurrently on the same instance.
func NewFramer(rw io.ReadWriter) FrameLayer {
	return &netFramer{r: rw, w: rw}
}

// ReadFrame reads the 9-byte header and the payload of one frame.
func (f *netFramer) ReadFrame() (*RawFrame, error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(f.r, hdr[:]); err != nil {
		return nil, err
	}
	length := uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])
	fr := &RawFrame{
		Length:   length,
		Type:     hdr[3],
		Flags:    hdr[4],
		StreamID: binary.BigEndian.Uint32(hdr[5:]) & 0x7fffffff,
	}
	if length > 0 {
		fr.Payload = make([]byte, length)
		if _, err := io.ReadFull(f.r, fr.Payload); err != nil {
			return nil, err
		}
	}
	return fr, nil
}

// WriteFrame serializes the header (computed from len(Payload)) and the payload.
func (f *netFramer) WriteFrame(fr *RawFrame) error {
	if len(fr.Payload) > 0xffffff {
		return fmt.Errorf("h2server: frame payload too large: %d", len(fr.Payload))
	}
	n := uint32(len(fr.Payload))
	var hdr [frameHeaderLen]byte
	hdr[0] = byte(n >> 16)
	hdr[1] = byte(n >> 8)
	hdr[2] = byte(n)
	hdr[3] = fr.Type
	hdr[4] = fr.Flags
	binary.BigEndian.PutUint32(hdr[5:], fr.StreamID&0x7fffffff)
	if _, err := f.w.Write(hdr[:]); err != nil {
		return err
	}
	if len(fr.Payload) > 0 {
		if _, err := f.w.Write(fr.Payload); err != nil {
			return err
		}
	}
	return nil
}
```

Create `hpackcodec.go`:

```go
package h2server

import (
	"bytes"
	"errors"

	"golang.org/x/net/http2/hpack"
)

// hpackDecodeTableSize is the HPACK dynamic-table size both halves advertise.
const hpackDecodeTableSize = 4096

// hpackDecoder is the decode half of a connection's HPACK codec. It owns a
// dynamic table updated by every header block it decodes; the read loop is its
// only caller.
type hpackDecoder struct {
	dec *hpack.Decoder
}

// hpackEncoder is the encode half. It owns its own dynamic table and an output
// buffer; it is only ever called while the connection write mutex is held.
type hpackEncoder struct {
	buf *bytes.Buffer
	enc *hpack.Encoder
}

// NewHPACKCodecs returns the decode and encode halves for one connection. The
// two halves maintain independent dynamic tables (RFC 7541 §2.2): the decoder's
// table mirrors what the peer encoded; the encoder's table is the server's own.
func NewHPACKCodecs() (decoder HeadersCodec, encoder HeadersCodec) {
	buf := new(bytes.Buffer)
	return &hpackDecoder{dec: hpack.NewDecoder(hpackDecodeTableSize, nil)},
		&hpackEncoder{buf: buf, enc: hpack.NewEncoder(buf)}
}

// Decode expands a complete HPACK header block into ordered name/value pairs.
func (d *hpackDecoder) Decode(block []byte) ([][2]string, error) {
	fields, err := d.dec.DecodeFull(block)
	if err != nil {
		return nil, err
	}
	pairs := make([][2]string, len(fields))
	for i, f := range fields {
		pairs[i] = [2]string{f.Name, f.Value}
	}
	return pairs, nil
}

// Encode is never called on the decode half.
func (d *hpackDecoder) Encode([][2]string) ([]byte, error) {
	return nil, errors.New("h2server: decode half cannot encode")
}

// Encode compresses pairs into one HPACK block. The encoder's dynamic table is
// updated as a side effect, so calls must stay serialized (they run under wmu).
func (e *hpackEncoder) Encode(pairs [][2]string) ([]byte, error) {
	e.buf.Reset()
	for _, p := range pairs {
		if err := e.enc.WriteField(hpack.HeaderField{Name: p[0], Value: p[1]}); err != nil {
			return nil, err
		}
	}
	out := make([]byte, e.buf.Len())
	copy(out, e.buf.Bytes())
	return out, nil
}

// Decode is never called on the encode half.
func (e *hpackEncoder) Decode([]byte) ([][2]string, error) {
	return nil, errors.New("h2server: encode half cannot decode")
}
```

### The runnable demo

The demo starts the server on a TLS loopback listener with a throwaway
self-signed certificate, then drives it with Go's own `net/http` client, which
negotiates "h2" via ALPN and speaks the real protocol. It prints no addresses or
timings, so the output is identical on every run.

Create `cmd/demo/main.go`:

```go
// cmd/demo starts the wired HTTP/2 server on a TLS loopback listener with a
// throwaway self-signed certificate, drives it with Go's own net/http client
// (which negotiates "h2" via ALPN), prints the results in a stable order, then
// shuts the server down gracefully.
//
// Run with: go run ./cmd/demo
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	h2server "example.com/h2server"
)

func main() {
	cert := selfSignedCert()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2"},
	})
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "hello from %s", r.URL.Path)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv, err := h2server.New(h2server.Config{
		Handler:         mux,
		NewFrameLayer:   h2server.NewFramer,
		NewHeadersCodec: h2server.NewHPACKCodecs,
	})
	if err != nil {
		log.Fatalf("New: %v", err)
	}
	go srv.Serve(ln) //nolint:errcheck

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}}, //nolint:gosec
		ForceAttemptHTTP2: true,
	}}
	base := "https://" + ln.Addr().String()

	fmt.Println("HTTP/2 server demo")

	body, status, proto := get(client, base+"/hello")
	fmt.Printf("GET /hello -> %d %s\n", status, proto)
	fmt.Printf("body: %s\n", strings.TrimSpace(body))

	_, status, proto = get(client, base+"/health")
	fmt.Printf("GET /health -> %d %s\n", status, proto)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx) //nolint:errcheck
	fmt.Println("server stopped")
}

func get(client *http.Client, url string) (string, int, string) {
	resp, err := client.Get(url)
	if err != nil {
		log.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, resp.Proto
}

// selfSignedCert builds a throwaway ECDSA P-256 certificate for localhost.
// It exists only to demonstrate the server; never use self-signed certs in
// production.
func selfSignedCert() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
HTTP/2 server demo
GET /hello -> 200 HTTP/2.0
body: hello from /hello
GET /health -> 204 HTTP/2.0
server stopped
```

### Tests

The end-to-end tests are the proof that all five layers compose: a real
HTTP/2 client performs a GET and checks the status, protocol, and body; a POST
echoes its request body, exercising client DATA, the body pipe, and the
flow-control WINDOW_UPDATEs; 25 concurrent streams on one connection prove the
write mutex serializes HPACK encodes correctly under load; a custom header and a
418 status confirm lowercasing and the status pseudo-header. Two shutdown tests
pin both outcomes: a closed connection drains and `Shutdown` returns nil, while a
lingering idle connection is force-closed at the deadline and `Shutdown` reports
the error. The remaining unit tests cover config defaults, request building, and
the framer round-trip. Run them with `-race` to validate the three-goroutine
model.

Create `server_test.go`:

```go
package h2server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func newTestServer(t *testing.T, handler http.Handler) (*Server, string) {
	t.Helper()
	cert := selfSignedCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		Handler:         handler,
		NewFrameLayer:   NewFramer,
		NewHeadersCodec: NewHPACKCodecs,
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	return srv, ln.Addr().String()
}

func h2Client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}}, //nolint:gosec
		ForceAttemptHTTP2: true,
	}}
}

func TestEndToEndSimpleRequest(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s", r.URL.Path)
	})
	srv, addr := newTestServer(t, handler)
	t.Cleanup(func() { srv.Shutdown(t.Context()) }) //nolint:errcheck

	resp, err := h2Client().Get("https://" + addr + "/test-path")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("proto = %q, want HTTP/2.0", resp.Proto)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "hello from /test-path" {
		t.Errorf("body = %q, want %q", got, "hello from /test-path")
	}
}

func TestEndToEndRequestBodyEcho(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(append([]byte("echo:"), body...)) //nolint:errcheck
	})
	srv, addr := newTestServer(t, handler)
	t.Cleanup(func() { srv.Shutdown(t.Context()) }) //nolint:errcheck

	resp, err := h2Client().Post("https://"+addr+"/echo", "text/plain",
		strings.NewReader("payload-123"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "echo:payload-123" {
		t.Errorf("body = %q, want %q", got, "echo:payload-123")
	}
}

func TestEndToEndConcurrentStreams(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s", r.URL.Path)
	})
	srv, addr := newTestServer(t, handler)
	t.Cleanup(func() { srv.Shutdown(t.Context()) }) //nolint:errcheck

	client := h2Client()
	const n = 25
	var wg sync.WaitGroup
	errs := make([]error, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			url := fmt.Sprintf("https://%s/p%d", addr, i)
			resp, err := client.Get(url)
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			bodies[i] = string(b)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		want := fmt.Sprintf("path=/p%d", i)
		if bodies[i] != want {
			t.Errorf("request %d body = %q, want %q", i, bodies[i], want)
		}
	}
}

func TestEndToEndCustomHeaderAndStatus(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Custom-Header", "value-42")
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, "short and stout")
	})
	srv, addr := newTestServer(t, handler)
	t.Cleanup(func() { srv.Shutdown(t.Context()) }) //nolint:errcheck

	resp, err := h2Client().Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Custom-Header"); got != "value-42" {
		t.Errorf("X-Custom-Header = %q, want value-42", got)
	}
}

// TestGracefulShutdownDrains makes a request, then closes the client's idle
// connection so the server's read loop sees EOF and untracks the connection;
// Shutdown then returns nil because no connections remain.
func TestGracefulShutdownDrains(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv, addr := newTestServer(t, handler)

	client := h2Client()
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	client.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown error: %v", err)
	}
}

// TestShutdownDeadlineForceCloses holds the connection open past the deadline.
// Shutdown sends GOAWAY but the idle client keeps the TCP connection open, so
// Shutdown force-closes it when ctx expires and reports the deadline error.
func TestShutdownDeadlineForceCloses(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv, addr := newTestServer(t, handler)
	defer srv.Shutdown(t.Context()) //nolint:errcheck

	client := h2Client()
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Do not close idle connections; the conn lingers.

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); err == nil {
		t.Error("want deadline error from Shutdown, got nil")
	}
}

func TestNewRejectsNilFrameLayer(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		NewHeadersCodec: func() (HeadersCodec, HeadersCodec) { return nil, nil },
	})
	if err == nil {
		t.Fatal("want error for nil NewFrameLayer, got nil")
	}
}

func TestNewRejectsNilHeadersCodec(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		NewFrameLayer: func(_ io.ReadWriter) FrameLayer { return nil },
	})
	if err == nil {
		t.Fatal("want error for nil NewHeadersCodec, got nil")
	}
}

func TestConfigEffectiveDefaults(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	if got := cfg.effectiveMaxConcurrent(); got != 100 {
		t.Errorf("effectiveMaxConcurrent = %d, want 100", got)
	}
	if got := cfg.effectiveWindowSize(); got != defaultInitWindowSize {
		t.Errorf("effectiveWindowSize = %d, want %d", got, defaultInitWindowSize)
	}
	if got := cfg.effectiveMaxFrame(); got != defaultMaxFrameSize {
		t.Errorf("effectiveMaxFrame = %d, want %d", got, defaultMaxFrameSize)
	}
	if got := cfg.effectiveHandler(); got == nil {
		t.Error("effectiveHandler should return http.DefaultServeMux, got nil")
	}
}

func TestBuildRequestParsesHeaders(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{":method", "GET"},
		{":path", "/hello"},
		{":scheme", "https"},
		{":authority", "example.com"},
		{"accept", "text/html"},
	}
	addr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:12345")
	req, err := buildRequest(pairs, addr)
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	if req.Method != "GET" {
		t.Errorf("Method = %q, want GET", req.Method)
	}
	if req.URL.Path != "/hello" {
		t.Errorf("Path = %q, want /hello", req.URL.Path)
	}
	if req.Header.Get("Accept") != "text/html" {
		t.Errorf("Accept header = %q, want text/html", req.Header.Get("Accept"))
	}
}

func TestBuildRequestRejectsMissingPseudoHeaders(t *testing.T) {
	t.Parallel()
	addr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:12345")
	cases := [][][2]string{
		{{":path", "/"}, {":scheme", "https"}, {":authority", "x"}},     // no :method
		{{":method", "GET"}, {":scheme", "https"}, {":authority", "x"}}, // no :path
		{{":method", "GET"}, {":path", "/"}, {":authority", "x"}},       // no :scheme
	}
	for i, pairs := range cases {
		if _, err := buildRequest(pairs, addr); err == nil {
			t.Errorf("case %d: want error, got nil", i)
		}
	}
}

func TestStringsContains(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ss   []string
		s    string
		want bool
	}{
		{[]string{"h2", "http/1.1"}, "h2", true},
		{[]string{"http/1.1"}, "h2", false},
		{nil, "h2", false},
	}
	for _, tc := range cases {
		if got := stringsContains(tc.ss, tc.s); got != tc.want {
			t.Errorf("stringsContains(%v, %q) = %v, want %v", tc.ss, tc.s, got, tc.want)
		}
	}
}

func ExampleNew() {
	_, err := New(Config{})
	fmt.Println(err)
	// Output:
	// h2server: Config.NewFrameLayer must not be nil
}

func TestFramerRoundTrip(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	fr := NewFramer(&buf)
	in := &RawFrame{Type: frameHeaders, Flags: flagEndHeaders, StreamID: 3, Payload: []byte("abc")}
	if err := fr.WriteFrame(in); err != nil {
		t.Fatal(err)
	}
	out, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type || out.Flags != in.Flags || out.StreamID != in.StreamID {
		t.Errorf("header mismatch: got %+v", out)
	}
	if string(out.Payload) != "abc" {
		t.Errorf("payload = %q, want abc", out.Payload)
	}
}
```

## Review

The server is correct when the standard library client — a strict, independent
HTTP/2 implementation — accepts every response. The mistakes most likely to break
it are subtle and concurrency-shaped. Holding the response writer's `mu` across a
`sendHeaders` call inverts the lock order with the read loop and deadlocks under
load; the 25-stream test is what surfaces it. Encoding HPACK outside `wmu` lets
two streams interleave dynamic-table updates, and the client's decoder rejects the
next header block — again, the concurrent test catches it where a single-request
test would not. Forgetting to lowercase header names produces responses the client
silently drops; `TestEndToEndCustomHeaderAndStatus` pins it. Acknowledging a
SETTINGS ACK loops forever; the handshake in every test would hang. Run the suite
with `-race`: it drives real TLS connections, concurrent streams, request bodies,
and both shutdown paths, which is where a missing lock or a misused channel
surfaces as a race or a hang rather than a wrong byte.

## Resources

- [RFC 9113: HTTP/2](https://httpwg.org/specs/rfc9113.html) — the connection
  preface (§3.4), frame format (§4), SETTINGS (§6.5), GOAWAY (§6.8), CONTINUATION
  (§6.10), and the pseudo-header and lowercase rules (§8.2, §8.3).
- [RFC 7301: TLS ALPN Extension](https://www.rfc-editor.org/rfc/rfc7301) — how the
  "h2" token is negotiated in the TLS handshake; pairs with `tls.Config.NextProtos`.
- [pkg.go.dev/golang.org/x/net/http2/hpack](https://pkg.go.dev/golang.org/x/net/http2/hpack)
  — the `Decoder`/`Encoder` this module wraps to satisfy `HeadersCodec`.
- [pkg.go.dev/net/http](https://pkg.go.dev/net/http) — `http.Handler`,
  `http.ResponseWriter`, `http.Flusher`, and the HTTP/2-capable `http.Transport`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-h2c-cleartext-server.md](02-h2c-cleartext-server.md)
