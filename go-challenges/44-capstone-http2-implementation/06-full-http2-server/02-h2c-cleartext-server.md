# Exercise 2: The Cleartext h2c Server

The wired server reached the network through TLS, where ALPN announces "h2"
before the first HTTP byte. This module reaches the same read loop over plain
cleartext TCP using HTTP/2 prior knowledge (h2c) — the TLS-less entry path of RFC
9113 §3.3, where a client opens a TCP connection and sends the connection preface
immediately, with no handshake and no negotiation.

The module is fully self-contained: it bundles the same proven server core
(framer, HPACK codec, connection read loop, response writer) and adds the one
piece that is genuinely different — a cleartext listener — then drives it with the
`golang.org/x/net/http2` transport in prior-knowledge mode. Nothing here imports
another exercise.

## What you'll build

```text
server.go          Config/Server/New + ListenAndServeTLS AND the new
                   ListenAndServeCleartext (plain TCP, no ALPN)
conn.go            the same read loop: preface, SETTINGS, frame switch, dispatch
response_writer.go the same per-stream http.ResponseWriter
framer.go          NewFramer: 9-byte frame header read/write
hpackcodec.go      NewHPACKCodecs: decode/encode halves over x/net/http2/hpack
cmd/
  demo/
    main.go        cleartext server driven by x/net/http2 prior-knowledge client
server_test.go     h2c GET, body echo, concurrent streams, bad-preface close,
                   graceful shutdown, request building
```

- Files: `server.go`, `conn.go`, `response_writer.go`, `framer.go`,
  `hpackcodec.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: `ListenAndServeCleartext`, which serves HTTP/2 over a plain TCP
  listener; the rest of the core is the wired server unchanged, because a non-TLS
  connection is already treated as h2c by the accept loop.
- Test: a prior-knowledge `http2.Transport` (AllowHTTP plus a plain dialer)
  performs a GET, a body echo, and 25 concurrent streams over cleartext; a raw
  TCP client that sends a malformed preface is closed; shutdown drains cleanly.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why prior knowledge, and why no Upgrade

HTTP/2 has two cleartext stories, and only one is still in the spec. RFC 7540
defined an HTTP/1.1 `Upgrade: h2c` handshake — the client sent an ordinary
HTTP/1.1 request with an `HTTP2-Settings` header and the server answered `101
Switching Protocols` before starting HTTP/2. RFC 9113 (the current HTTP/2 spec)
*removed* that mechanism. What remains for cleartext is "prior knowledge" (RFC
9113 §3.3): the client already knows the server speaks HTTP/2, so it opens a TCP
connection and sends the 24-byte connection preface as its very first bytes — the
same preface used over TLS. No `Upgrade`, no `101`, no `HTTP2-Settings` header.

This is why the cleartext path needs almost no new code. The accept loop already
inspects the connection: if it is a `*tls.Conn` it runs the handshake and checks
ALPN; otherwise it assumes h2c and proceeds straight to the read loop, which reads
the preface. The only thing missing is a convenience that opens a plain TCP
listener instead of a TLS one. Operationally, h2c carries no confidentiality and
no server authentication, so it belongs on a loopback interface, inside a trusted
mesh, or behind a TLS-terminating proxy that re-speaks h2c to the backend — never
directly on the open internet.

`ListenAndServeCleartext` is the whole addition: open a TCP listener and hand it
to the existing `Serve`. Everything downstream — preface validation, the SETTINGS
handshake, the frame switch, HPACK, the response writer, graceful shutdown — is
the wired server from exercise 1, bundled here unchanged so this module stands
alone.

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
// (RFC 9113 §3.4). It is identical for TLS ("h2") and cleartext ("h2c") starts.
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
// ReadFrame and WriteFrame are never called concurrently on the same instance.
type FrameLayer interface {
	ReadFrame() (*RawFrame, error)
	WriteFrame(f *RawFrame) error
}

// HeadersCodec encodes and decodes HPACK header block fragments.
// Decode is called only by the read loop; Encode is called only while wmu is held.
type HeadersCodec interface {
	Decode(block []byte) ([][2]string, error)
	Encode(pairs [][2]string) ([]byte, error)
}

// FrameLayerFactory creates a FrameLayer for a new connection.
type FrameLayerFactory func(rw io.ReadWriter) FrameLayer

// HeadersCodecFactory creates decoder and encoder halves for a new connection.
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

	// InitialWindowSize is the per-stream flow-control window. Zero defaults to
	// the RFC 9113 minimum of 65535.
	InitialWindowSize uint32

	// MaxFrameSize is the maximum DATA payload the server will accept.
	// Zero defaults to the RFC 9113 minimum of 16384.
	MaxFrameSize uint32

	// ReadTimeout bounds each frame read. Zero means no timeout.
	ReadTimeout time.Duration

	// IdleTimeout closes a connection idle for longer than this value.
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

// ListenAndServeCleartext opens a plain TCP listener on addr and serves HTTP/2
// over cleartext using prior knowledge (RFC 9113 §3.3): there is no TLS
// handshake and no ALPN, so every accepted client is assumed to open with the
// connection preface. Use only on trusted networks; cleartext HTTP/2 offers no
// confidentiality.
func (s *Server) ListenAndServeCleartext(addr string) error {
	ln, err := net.Listen("tcp", addr)
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

		// A *tls.Conn negotiates "h2" via ALPN; any other connection is taken
		// as cleartext h2c and served directly (prior knowledge).
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

### The shared connection core

The read loop, response writer, framer, and HPACK adapter are byte-for-byte the
wired server's. They are reproduced here so the module stands alone; the prose for
each lives in exercise 1. The one thing worth re-stating in the cleartext context:
`readPreface` reads the 24 bytes straight from the connection before the first
`ReadFrame`, and the framer is unbuffered, so over plain TCP — where there is no
TLS record layer to absorb stray bytes — the preface and the first SETTINGS frame
are read in exact sequence with nothing lost between them.

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

// serve reads the preface, exchanges SETTINGS, and loops over frames.
func (c *h2conn) serve() {
	defer c.nc.Close()

	if err := c.readPreface(); err != nil {
		return
	}
	if err := c.sendServerSettings(); err != nil {
		return
	}

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

// sendServerSettings sends the server's initial SETTINGS frame (RFC 9113 §6.5).
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
		c.sendWindowUpdate(0, uint32(len(f.Payload)))
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

// writeFrame serializes all frame writes (and the HPACK encode in sendHeaders)
// through wmu, so encode+write is always atomic.
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
	bodyW *io.PipeWriter
	rw    *h2responseWriter

	sendWin atomic.Int32
	winCh   chan struct{}
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

func (w *h2responseWriter) Header() http.Header {
	return w.header
}

// WriteHeader sends a HEADERS frame with the given status code; first call wins.
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

// Write implicitly writes a 200 header on first use, then sends p as DATA.
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

// Flush implements http.Flusher; each Write already sends a DATA frame.
func (w *h2responseWriter) Flush() {}

// finalize closes the stream with END_STREAM if the handler did not.
func (w *h2responseWriter) finalize() {
	w.mu.Lock()
	wroteHeader := w.wroteHeader
	sentEnd := w.sentEndStream
	w.mu.Unlock()

	if !wroteHeader {
		_ = w.sendHeaders(http.StatusOK, true)
		return
	}
	if !sentEnd {
		_ = w.conn.writeFrame(&RawFrame{
			Type:     frameData,
			Flags:    flagEndStream,
			StreamID: w.streamID,
		})
	}
}

// sendHeaders encodes pairs with HPACK and sends a HEADERS frame under wmu.
// :status is first (RFC 9113 §8.3.2); field names are lowercased (§8.2.1).
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

// sendData sends p as DATA frames bounded by MAX_FRAME_SIZE.
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

// NewFramer returns a FrameLayer reading and writing RFC 9113 §4.1 frames on rw.
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

// hpackDecoder is the decode half; the read loop is its only caller.
type hpackDecoder struct {
	dec *hpack.Decoder
}

// hpackEncoder is the encode half; it runs only under the connection write mutex.
type hpackEncoder struct {
	buf *bytes.Buffer
	enc *hpack.Encoder
}

// NewHPACKCodecs returns the decode and encode halves for one connection, each
// with its own dynamic table (RFC 7541 §2.2).
func NewHPACKCodecs() (decoder HeadersCodec, encoder HeadersCodec) {
	buf := new(bytes.Buffer)
	return &hpackDecoder{dec: hpack.NewDecoder(hpackDecodeTableSize, nil)},
		&hpackEncoder{buf: buf, enc: hpack.NewEncoder(buf)}
}

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

func (d *hpackDecoder) Encode([][2]string) ([]byte, error) {
	return nil, errors.New("h2server: decode half cannot encode")
}

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

func (e *hpackEncoder) Decode([]byte) ([][2]string, error) {
	return nil, errors.New("h2server: encode half cannot decode")
}
```

### The runnable demo

The demo starts the cleartext server on a loopback TCP listener and drives it
with the `golang.org/x/net/http2` transport in prior-knowledge mode: `AllowHTTP`
lets the transport use an `http://` URL, and a plain (non-TLS) dialer wired into
`DialTLSContext` makes it send the HTTP/2 preface straight onto the cleartext
socket. The output is identical on every run.

Create `cmd/demo/main.go`:

```go
// cmd/demo starts the cleartext HTTP/2 (h2c) server on a loopback TCP listener,
// drives it with the x/net/http2 transport in prior-knowledge mode (no TLS, no
// ALPN; the client sends the connection preface immediately), prints the
// results in a stable order, then shuts the server down.
//
// Run with: go run ./cmd/demo
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http2"

	h2server "example.com/h2cserver"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

	// Prior-knowledge h2c: AllowHTTP plus a plain (non-TLS) dialer.
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, network, addr)
		},
	}
	client := &http.Client{Transport: tr}
	base := "http://" + ln.Addr().String()

	fmt.Println("h2c (cleartext HTTP/2) server demo")

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
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
h2c (cleartext HTTP/2) server demo
GET /hello -> 200 HTTP/2.0
body: hello from /hello
GET /health -> 204 HTTP/2.0
server stopped
```

### Tests

The tests prove the server speaks real h2c. A prior-knowledge `http2.Transport`
performs a GET (and checks `ProtoMajor == 2`, confirming the cleartext connection
actually negotiated HTTP/2 and did not silently fall back), echoes a request body,
and drives 25 concurrent streams over one cleartext connection. A raw TCP client
that writes a malformed preface is closed by the server, validating the §3.4
preface check on the plain socket. Shutdown drains a closed connection cleanly.
Run with `-race` to validate the shared three-goroutine core over cleartext.

Create `server_test.go`:

```go
package h2server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// newCleartextServer starts an h2c server on a loopback TCP listener and returns
// it with its address.
func newCleartextServer(t *testing.T, handler http.Handler) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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

// h2cClient returns an http.Client whose transport speaks prior-knowledge h2c:
// AllowHTTP lets it use an "http" scheme, and the plain dialer means it sends
// the HTTP/2 preface straight onto a cleartext TCP connection.
func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, network, addr)
		},
	}}
}

func TestH2CSimpleRequest(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s", r.URL.Path)
	})
	srv, addr := newCleartextServer(t, handler)
	t.Cleanup(func() { srv.Shutdown(t.Context()) }) //nolint:errcheck

	resp, err := h2cClient().Get("http://" + addr + "/test-path")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Errorf("ProtoMajor = %d, want 2", resp.ProtoMajor)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "hello from /test-path" {
		t.Errorf("body = %q, want %q", got, "hello from /test-path")
	}
}

func TestH2CRequestBodyEcho(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(append([]byte("echo:"), body...)) //nolint:errcheck
	})
	srv, addr := newCleartextServer(t, handler)
	t.Cleanup(func() { srv.Shutdown(t.Context()) }) //nolint:errcheck

	resp, err := h2cClient().Post("http://"+addr+"/echo", "text/plain",
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

func TestH2CConcurrentStreams(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s", r.URL.Path)
	})
	srv, addr := newCleartextServer(t, handler)
	t.Cleanup(func() { srv.Shutdown(t.Context()) }) //nolint:errcheck

	client := h2cClient()
	const n = 25
	var wg sync.WaitGroup
	errs := make([]error, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Get(fmt.Sprintf("http://%s/p%d", addr, i))
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
		if want := fmt.Sprintf("path=/p%d", i); bodies[i] != want {
			t.Errorf("request %d body = %q, want %q", i, bodies[i], want)
		}
	}
}

// TestH2CRejectsBadPreface confirms the server validates the §3.4 preface on a
// plain socket: junk where the 24-byte preface should be closes the connection.
func TestH2CRejectsBadPreface(t *testing.T) {
	t.Parallel()
	srv, addr := newCleartextServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(func() { srv.Shutdown(t.Context()) }) //nolint:errcheck

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("NOT-A-VALID-HTTP2-PREFACE")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(time.Second)) //nolint:errcheck
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Error("want connection closed after bad preface, got data")
	}
}

func TestGracefulShutdownDrains(t *testing.T) {
	srv, addr := newCleartextServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	client := h2cClient()
	resp, err := client.Get("http://" + addr + "/")
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

func TestBuildRequestParsesPseudoHeaders(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{":method", "GET"},
		{":path", "/hello"},
		{":scheme", "http"},
		{":authority", "example.com"},
	}
	addr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:12345")
	req, err := buildRequest(pairs, addr)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "GET" || req.URL.Path != "/hello" {
		t.Errorf("got method=%q path=%q", req.Method, req.URL.Path)
	}
}

func ExampleNew() {
	_, err := New(Config{})
	fmt.Println(err)
	// Output:
	// h2server: Config.NewFrameLayer must not be nil
}
```

## Review

The cleartext server is correct when the `x/net/http2` transport — speaking real
prior-knowledge h2c — accepts every response and reports HTTP/2. The mistake most
specific to this module is assuming cleartext needs the old `Upgrade: h2c`
handshake; it does not, because RFC 9113 dropped that path and the server reads
the preface straight from the socket. The bad-preface test guards the §3.4 check:
without it, a server that skipped validation would try to parse arbitrary bytes as
a frame header and behave unpredictably. Everything else that can break is the
shared core's — lock ordering, HPACK-under-`wmu`, lowercase header names — and the
concurrent-streams test exercises it the same way over cleartext. Run the suite
with `-race`; the only difference from exercise 1 is the absence of TLS, which
makes the preface and SETTINGS sequencing on a raw socket the thing to watch.

## Resources

- [RFC 9113 §3.3: Starting HTTP/2 with Prior Knowledge](https://httpwg.org/specs/rfc9113.html#known-http)
  — the cleartext h2c entry path this module implements (and the note that the
  RFC 7540 Upgrade mechanism was removed).
- [RFC 9113 §3.4: HTTP/2 Connection Preface](https://httpwg.org/specs/rfc9113.html#ConnectionHeader)
  — the 24-byte sequence the server validates on the plain socket.
- [pkg.go.dev/golang.org/x/net/http2](https://pkg.go.dev/golang.org/x/net/http2)
  — `Transport.AllowHTTP` and `DialTLSContext`, the prior-knowledge h2c client
  used in the demo and tests.
- [pkg.go.dev/net#Listen](https://pkg.go.dev/net#Listen) — the plain TCP listener
  behind `ListenAndServeCleartext`.

---

Back to [01-wired-http2-server.md](01-wired-http2-server.md) | Next: [Partitioned Storage Engine](../../45-capstone-distributed-key-value-store/01-partitioned-storage/01-partitioned-storage.md)
