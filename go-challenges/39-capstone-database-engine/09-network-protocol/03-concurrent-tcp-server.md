# Exercise 3: Concurrent TCP Server with Graceful Shutdown

A wire protocol is only useful behind a listener that accepts many clients at once and shuts down without dropping in-flight work. This exercise builds that server: it accepts TCP connections, runs the startup handshake on each, hands the session to an application `Handler`, gives every connection its own `ConnState`, and on shutdown stops accepting, waits for active sessions to drain, and bounds that wait with a timeout. The server is split into `Serve(ctx, listener)` so a test can run it on an ephemeral port and drive a real client through it.

This module is fully self-contained. It depends only on the standard library and ships its own demo and tests, including a duplicated copy of the framing and startup scaffolding so it builds with no dependency on any other exercise.

## What you'll build

```text
wire.go              Framer scaffolding, ReadStartup, message-type constants, ProtocolVersion3
startup.go           SendStartupResponse and its component messages
simple.go            ColumnDesc, SendRowDescription/SendDataRow/SendCmdComplete, CommandTag
error.go             WireError, SendErrorResponse, SQLSTATE constants
state.go             ConnState (per-connection prepared statements, portals, tx status)
server.go            Server, NewServer, options, Serve, ListenAndServe, ActiveConnections
cmd/
  demo/
    main.go          start a server on 127.0.0.1:0, run one client, shut down gracefully
server_test.go       a real TCP round-trip, per-connection state isolation, graceful shutdown
```

- Files: `wire.go`, `startup.go`, `simple.go`, `error.go`, `state.go`, `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: `Server`, `NewServer`, `WithShutdownTimeout`, `WithLogger`, `(*Server).Serve`, `(*Server).ListenAndServe`, `(*Server).ActiveConnections`, and the per-connection `serveConn`.
- Test: `server_test.go` drives a client over a real TCP socket through the handshake and a query, asserts two concurrent connections do not share state, and that `Serve` returns when its context is cancelled.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p pgserver/cmd/demo && cd pgserver
go mod init example.com/pgserver
```

### How the server stays correct under concurrency

`Serve` runs an accept loop in its own goroutine: each accepted connection gets a fresh goroutine and a fresh `ConnState`, registered with a `sync.WaitGroup`. Per-connection protocol state is never shared — a single shared `ConnState` would let one client see another's prepared statements and portals, a logical corruption a mutex cannot fix. The only shared mutable state is the active-connection count, and that is the one thing the `sync.Mutex` guards; it is bookkeeping, not protocol state.

Shutdown is cooperative and ordered. `Serve` blocks on `ctx.Done()`; when the context is cancelled it closes the listener, which makes the in-flight `Accept` return an error. The accept goroutine distinguishes that expected error (it checks `ctx.Done()` first and reports `nil`) from a real accept failure. Then `Serve` waits on the `WaitGroup` for active sessions to finish, but only up to the configured timeout — past that it returns and lets the process exit rather than hanging on a stuck client. Wiring an OS signal to shutdown is a one-liner at the call site with `signal.NotifyContext(ctx, os.Interrupt)`, which cancels the context on SIGINT/SIGTERM; the server itself only knows about the context, which keeps it testable.

`serveConn` runs the per-connection lifecycle: bump the active count, read the startup message, reject any protocol version other than 3.0 with a `FATAL` `ErrorResponse` (a client must never be left guessing), send the startup response, then call the application `Handler` and loop until the client disconnects. A clean disconnect surfaces as `io.EOF` or `net.ErrClosed` from the next read, and `serveConn` treats those as normal end-of-session rather than errors to log.

Create `wire.go`:

```go
// Package wire implements a concurrent TCP server for a simplified PostgreSQL
// wire protocol: length-prefixed framing, the startup handshake, and a
// per-connection session handler. Every typed message is
// type(1) + int32(length, includes itself) + payload, all integers big-endian.
package wire

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// Message type bytes.
const (
	MsgQuery     byte = 'Q' // frontend
	MsgTerminate byte = 'X' // frontend

	MsgAuth           byte = 'R'
	MsgParamStatus    byte = 'S'
	MsgBackendKeyData byte = 'K'
	MsgReadyForQuery  byte = 'Z'
	MsgErrorResponse  byte = 'E'
	MsgRowDescription byte = 'T'
	MsgDataRow        byte = 'D'
	MsgCmdComplete    byte = 'C'
)

// Transaction status bytes for ReadyForQuery.
const (
	TxIdle   byte = 'I'
	TxInTx   byte = 'T'
	TxFailed byte = 'E'
)

// ProtocolVersion3 is startup protocol version 3.0.
const ProtocolVersion3 int32 = 196608

// Framer wraps a net.Conn with buffered I/O and reads/writes wire messages.
type Framer struct {
	r   *bufio.Reader
	w   *bufio.Writer
	raw net.Conn
}

// NewFramer wraps conn in a Framer.
func NewFramer(conn net.Conn) *Framer {
	return &Framer{r: bufio.NewReader(conn), w: bufio.NewWriter(conn), raw: conn}
}

// ReadMessage reads one typed message: type(1) | int32(len) | payload(len-4).
func (f *Framer) ReadMessage() (msgType byte, payload []byte, err error) {
	msgType, err = f.r.ReadByte()
	if err != nil {
		return 0, nil, fmt.Errorf("wire: read type: %w", err)
	}
	var lenBuf [4]byte
	if _, err = io.ReadFull(f.r, lenBuf[:]); err != nil {
		return 0, nil, fmt.Errorf("wire: read length: %w", err)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n < 4 {
		return 0, nil, fmt.Errorf("wire: message length %d < 4", n)
	}
	payload = make([]byte, n-4)
	if _, err = io.ReadFull(f.r, payload); err != nil {
		return 0, nil, fmt.Errorf("wire: read payload: %w", err)
	}
	return msgType, payload, nil
}

// ReadStartup reads the typeless startup message.
func (f *Framer) ReadStartup() (version int32, params map[string]string, err error) {
	var lenBuf [4]byte
	if _, err = io.ReadFull(f.r, lenBuf[:]); err != nil {
		return 0, nil, fmt.Errorf("wire: startup length: %w", err)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n < 8 {
		return 0, nil, fmt.Errorf("wire: startup length %d < 8", n)
	}
	payload := make([]byte, n-4)
	if _, err = io.ReadFull(f.r, payload); err != nil {
		return 0, nil, fmt.Errorf("wire: startup payload: %w", err)
	}
	version = int32(binary.BigEndian.Uint32(payload[:4]))
	params = parseKeyValue(payload[4:])
	return version, params, nil
}

// WriteMessage writes one typed message. Buffered: call Flush to send.
func (f *Framer) WriteMessage(msgType byte, payload []byte) error {
	if err := f.w.WriteByte(msgType); err != nil {
		return fmt.Errorf("wire: write type: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)+4))
	if _, err := f.w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("wire: write length: %w", err)
	}
	if len(payload) > 0 {
		if _, err := f.w.Write(payload); err != nil {
			return fmt.Errorf("wire: write payload: %w", err)
		}
	}
	return nil
}

// WriteStartup writes a client-side startup message and flushes.
func WriteStartup(f *Framer, params map[string]string) error {
	var body []byte
	body = binary.BigEndian.AppendUint32(body, uint32(ProtocolVersion3))
	for k, v := range params {
		body = append(body, k...)
		body = append(body, 0)
		body = append(body, v...)
		body = append(body, 0)
	}
	body = append(body, 0)
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)+4))
	if _, err := f.w.Write(header[:]); err != nil {
		return fmt.Errorf("wire: startup header: %w", err)
	}
	if _, err := f.w.Write(body); err != nil {
		return fmt.Errorf("wire: startup body: %w", err)
	}
	return f.Flush()
}

// Flush flushes buffered writes.
func (f *Framer) Flush() error { return f.w.Flush() }

// Close closes the underlying connection.
func (f *Framer) Close() error { return f.raw.Close() }

func parseKeyValue(data []byte) map[string]string {
	m := make(map[string]string)
	for len(data) > 0 {
		key, rest, ok := readCString(data)
		if !ok || key == "" {
			break
		}
		data = rest
		val, rest2, _ := readCString(data)
		data = rest2
		m[key] = val
	}
	return m
}

func readCString(data []byte) (s string, rest []byte, ok bool) {
	for i, b := range data {
		if b == 0 {
			return string(data[:i]), data[i+1:], true
		}
	}
	return "", data, false
}

// ReadCString is the exported form for external callers.
func ReadCString(data []byte) (s string, rest []byte, ok bool) {
	return readCString(data)
}
```

Create `startup.go`:

```go
package wire

import (
	"encoding/binary"
	"fmt"
)

func (f *Framer) sendAuthOK() error {
	var payload [4]byte
	return f.WriteMessage(MsgAuth, payload[:])
}

func (f *Framer) sendParamStatus(name, value string) error {
	payload := append([]byte(name), 0)
	payload = append(payload, value...)
	payload = append(payload, 0)
	return f.WriteMessage(MsgParamStatus, payload)
}

func (f *Framer) sendBackendKeyData(pid, secret int32) error {
	var payload [8]byte
	binary.BigEndian.PutUint32(payload[0:], uint32(pid))
	binary.BigEndian.PutUint32(payload[4:], uint32(secret))
	return f.WriteMessage(MsgBackendKeyData, payload[:])
}

// SendReadyForQuery sends ReadyForQuery with the given transaction status byte.
func (f *Framer) SendReadyForQuery(txStatus byte) error {
	return f.WriteMessage(MsgReadyForQuery, []byte{txStatus})
}

// SendStartupResponse performs the full startup response and flushes.
func (f *Framer) SendStartupResponse(pid, secret int32) error {
	if err := f.sendAuthOK(); err != nil {
		return fmt.Errorf("startup: auth ok: %w", err)
	}
	for _, p := range [][2]string{
		{"server_version", "15.0"},
		{"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"},
	} {
		if err := f.sendParamStatus(p[0], p[1]); err != nil {
			return fmt.Errorf("startup: param status %q: %w", p[0], err)
		}
	}
	if err := f.sendBackendKeyData(pid, secret); err != nil {
		return fmt.Errorf("startup: backend key data: %w", err)
	}
	if err := f.SendReadyForQuery(TxIdle); err != nil {
		return fmt.Errorf("startup: ready for query: %w", err)
	}
	return f.Flush()
}
```

Create `simple.go`:

```go
package wire

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// ColumnDesc describes one result column.
type ColumnDesc struct {
	Name    string
	TypeOID uint32
}

// Common PostgreSQL type OIDs.
const (
	OIDText uint32 = 25
	OIDInt4 uint32 = 23
)

// SendRowDescription sends a RowDescription (text format for every column).
func (f *Framer) SendRowDescription(cols []ColumnDesc) error {
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(cols)))
	for _, col := range cols {
		payload = append(payload, col.Name...)
		payload = append(payload, 0)
		payload = binary.BigEndian.AppendUint32(payload, 0)           // table OID
		payload = binary.BigEndian.AppendUint16(payload, 0)           // column attr number
		payload = binary.BigEndian.AppendUint32(payload, col.TypeOID) // type OID
		payload = binary.BigEndian.AppendUint16(payload, 0xFFFF)      // type size (var-length)
		payload = binary.BigEndian.AppendUint32(payload, 0xFFFFFFFF)  // type modifier (-1)
		payload = binary.BigEndian.AppendUint16(payload, 0)           // format code (text)
	}
	return f.WriteMessage(MsgRowDescription, payload)
}

// SendDataRow sends one DataRow. A nil value encodes as SQL NULL (length -1).
func (f *Framer) SendDataRow(values []*string) error {
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(values)))
	for _, v := range values {
		if v == nil {
			payload = binary.BigEndian.AppendUint32(payload, 0xFFFFFFFF)
		} else {
			b := []byte(*v)
			payload = binary.BigEndian.AppendUint32(payload, uint32(len(b)))
			payload = append(payload, b...)
		}
	}
	return f.WriteMessage(MsgDataRow, payload)
}

// SendCmdComplete sends CommandComplete with the given tag.
func (f *Framer) SendCmdComplete(tag string) error {
	return f.WriteMessage(MsgCmdComplete, append([]byte(tag), 0))
}

// StringPtr returns a pointer to a copy of s.
func StringPtr(s string) *string { return &s }

// CommandTag builds the CommandComplete tag for a SQL command verb.
func CommandTag(cmd string, rowsAffected int) string {
	switch strings.ToUpper(cmd) {
	case "SELECT":
		return fmt.Sprintf("SELECT %d", rowsAffected)
	case "INSERT":
		return fmt.Sprintf("INSERT 0 %d", rowsAffected)
	case "UPDATE":
		return fmt.Sprintf("UPDATE %d", rowsAffected)
	case "DELETE":
		return fmt.Sprintf("DELETE %d", rowsAffected)
	default:
		return cmd
	}
}
```

Create `error.go`:

```go
package wire

import "fmt"

// SQLSTATE codes used by the server.
const (
	SQLStateFeatureNotSupported = "0A000"
	SQLStateInternalError       = "XX000"
)

// WireError is a structured protocol error that serializes into an ErrorResponse.
type WireError struct {
	Severity string
	Code     string
	Message  string
}

// Error implements the error interface.
func (e *WireError) Error() string { return e.Message }

// SendErrorResponse encodes and sends an ErrorResponse and flushes.
func (f *Framer) SendErrorResponse(we *WireError) error {
	var payload []byte
	appendField := func(code byte, value string) {
		if value == "" {
			return
		}
		payload = append(payload, code)
		payload = append(payload, value...)
		payload = append(payload, 0)
	}
	appendField('S', we.Severity)
	appendField('V', we.Severity)
	appendField('C', we.Code)
	appendField('M', we.Message)
	payload = append(payload, 0)
	if err := f.WriteMessage(MsgErrorResponse, payload); err != nil {
		return fmt.Errorf("error response: %w", err)
	}
	return f.Flush()
}
```

Create `state.go`:

```go
package wire

// PreparedStatement is the result of a Parse: a named query with parameter OIDs.
type PreparedStatement struct {
	Name       string
	Query      string
	ParamTypes []uint32
}

// Portal is the result of a Bind: a statement with concrete parameter values.
type Portal struct {
	Name      string
	Statement *PreparedStatement
	Params    [][]byte
}

// ConnState holds one connection's prepared statements, portals, and tx status.
// Every connection has its own ConnState; it is never shared between connections.
type ConnState struct {
	Statements map[string]*PreparedStatement
	Portals    map[string]*Portal
	TxStatus   byte
}

// NewConnState allocates a fresh ConnState with idle transaction status.
func NewConnState() *ConnState {
	return &ConnState{
		Statements: make(map[string]*PreparedStatement),
		Portals:    make(map[string]*Portal),
		TxStatus:   TxIdle,
	}
}
```

Create `server.go`:

```go
package wire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// Handler processes one client session after the startup handshake. It reads
// frontend messages and writes backend messages until the client disconnects
// or ctx is done.
type Handler func(ctx context.Context, f *Framer, state *ConnState) error

// Server accepts TCP connections and dispatches each to a Handler.
type Server struct {
	handler  Handler
	logger   *slog.Logger
	shutdown time.Duration
	pid      int32

	mu     sync.Mutex
	active int
}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithShutdownTimeout bounds how long Serve waits for in-flight sessions.
func WithShutdownTimeout(d time.Duration) ServerOption {
	return func(s *Server) { s.shutdown = d }
}

// WithLogger sets the structured logger for connection-level events.
func WithLogger(l *slog.Logger) ServerOption {
	return func(s *Server) { s.logger = l }
}

// NewServer creates a Server with the given handler and options.
func NewServer(handler Handler, opts ...ServerOption) *Server {
	s := &Server{
		handler:  handler,
		logger:   slog.Default(),
		shutdown: 30 * time.Second,
		pid:      int32(os.Getpid()),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ListenAndServe listens on addr and serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", addr, err)
	}
	return s.Serve(ctx, l)
}

// Serve accepts connections on l until ctx is cancelled, then closes the
// listener and waits up to the shutdown timeout for active sessions to drain.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	s.logger.Info("pgwire: serving", "addr", l.Addr())

	var wg sync.WaitGroup
	acceptErr := make(chan error, 1)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					acceptErr <- nil
				default:
					acceptErr <- fmt.Errorf("server: accept: %w", err)
				}
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				s.serveConn(ctx, c)
			}(conn)
		}
	}()

	<-ctx.Done()
	s.logger.Info("pgwire: shutdown, closing listener")
	_ = l.Close()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(s.shutdown):
		s.logger.Warn("pgwire: shutdown timeout exceeded, leaving sessions active")
	}
	return <-acceptErr
}

// serveConn handles one client: startup handshake then handler invocation.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	s.mu.Lock()
	s.active++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()

	f := NewFramer(conn)
	state := NewConnState()

	version, params, err := f.ReadStartup()
	if err != nil {
		s.logger.Error("pgwire: startup read", "err", err)
		return
	}
	if version != ProtocolVersion3 {
		_ = f.SendErrorResponse(&WireError{
			Severity: "FATAL",
			Code:     SQLStateFeatureNotSupported,
			Message:  fmt.Sprintf("unsupported protocol version %d", version),
		})
		return
	}
	s.logger.Info("pgwire: client connected", "user", params["user"], "database", params["database"])

	if err := f.SendStartupResponse(s.pid, 0); err != nil {
		s.logger.Error("pgwire: startup response", "err", err)
		return
	}

	if err := s.handler(ctx, f, state); err != nil &&
		!errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		s.logger.Error("pgwire: handler error", "err", err)
	}
}

// ActiveConnections returns the current number of live sessions.
func (s *Server) ActiveConnections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}
```

### The runnable demo

The demo starts a server on an ephemeral port (`127.0.0.1:0`), runs one client through the full conversation, and then cancels the context to shut the server down. Server logging is routed to `io.Discard` so the output is exactly the demo's own lines. The handler answers a `Query` with a single-row result; the client prints the row, then closes and triggers shutdown.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"

	"example.com/pgserver"
)

func handle(ctx context.Context, f *wire.Framer, state *wire.ConnState) error {
	_ = ctx
	_ = state
	for {
		mt, payload, err := f.ReadMessage()
		if err != nil {
			return err
		}
		switch mt {
		case wire.MsgQuery:
			q, _, _ := wire.ReadCString(payload)
			fmt.Printf("[server] received query: %q\n", q)
			if err := f.SendRowDescription([]wire.ColumnDesc{{Name: "n", TypeOID: wire.OIDInt4}}); err != nil {
				return err
			}
			if err := f.SendDataRow([]*string{wire.StringPtr("1")}); err != nil {
				return err
			}
			if err := f.SendCmdComplete(wire.CommandTag("SELECT", 1)); err != nil {
				return err
			}
			if err := f.SendReadyForQuery(wire.TxIdle); err != nil {
				return err
			}
			if err := f.Flush(); err != nil {
				return err
			}
		case wire.MsgTerminate:
			return nil
		}
	}
}

func main() {
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := wire.NewServer(handle, wire.WithLogger(silent))

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, l) }()

	if err := runClient(l.Addr().String()); err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		cancel()
		os.Exit(1)
	}

	fmt.Println("[client] done; shutting server down")
	cancel()
	if err := <-served; err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
	}
	fmt.Println("[server] stopped")
}

func runClient(addr string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	f := wire.NewFramer(conn)

	if err := wire.WriteStartup(f, map[string]string{"user": "demo", "database": "testdb"}); err != nil {
		return err
	}
	if err := readUntilReady(f); err != nil {
		return err
	}

	if err := f.WriteMessage(wire.MsgQuery, append([]byte("SELECT 1"), 0)); err != nil {
		return err
	}
	if err := f.Flush(); err != nil {
		return err
	}
	for {
		mt, payload, err := f.ReadMessage()
		if err != nil {
			return err
		}
		switch mt {
		case wire.MsgDataRow:
			valLen := int(binary.BigEndian.Uint32(payload[2:6]))
			fmt.Printf("[client] DataRow n=%s\n", payload[6:6+valLen])
		case wire.MsgCmdComplete:
			tag, _, _ := wire.ReadCString(payload)
			fmt.Printf("[client] CommandComplete %q\n", tag)
		case wire.MsgReadyForQuery:
			return f.WriteMessage(wire.MsgTerminate, nil)
		}
	}
}

func readUntilReady(f *wire.Framer) error {
	for {
		mt, _, err := f.ReadMessage()
		if err != nil {
			return err
		}
		if mt == wire.MsgReadyForQuery {
			return nil
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[server] received query: "SELECT 1"
[client] DataRow n=1
[client] CommandComplete "SELECT 1"
[client] done; shutting server down
[server] stopped
```

### Tests

The tests run a real listener on `127.0.0.1:0`, which avoids a hard-coded port. A round-trip test drives a client through startup and a query and checks the row comes back. An isolation test opens two connections, has the handler stash a per-connection statement keyed by the user name, and asserts neither connection sees the other's state. A shutdown test cancels the context and asserts `Serve` returns promptly.

Create `server_test.go`:

```go
package wire

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func silentServer(h Handler) *Server {
	return NewServer(h, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), WithShutdownTimeout(2*time.Second))
}

func startupClient(t *testing.T, addr, user string) *Framer {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	f := NewFramer(conn)
	if err := WriteStartup(f, map[string]string{"user": user, "database": "db"}); err != nil {
		t.Fatalf("startup: %v", err)
	}
	for {
		mt, _, err := f.ReadMessage()
		if err != nil {
			t.Fatalf("read startup: %v", err)
		}
		if mt == MsgReadyForQuery {
			return f
		}
	}
}

func TestServerQueryRoundTrip(t *testing.T) {
	t.Parallel()
	handler := func(ctx context.Context, f *Framer, state *ConnState) error {
		for {
			mt, _, err := f.ReadMessage()
			if err != nil {
				return err
			}
			if mt != MsgQuery {
				continue
			}
			if err := f.SendRowDescription([]ColumnDesc{{Name: "n", TypeOID: OIDInt4}}); err != nil {
				return err
			}
			if err := f.SendDataRow([]*string{StringPtr("7")}); err != nil {
				return err
			}
			if err := f.SendCmdComplete(CommandTag("SELECT", 1)); err != nil {
				return err
			}
			if err := f.SendReadyForQuery(TxIdle); err != nil {
				return err
			}
			if err := f.Flush(); err != nil {
				return err
			}
		}
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- silentServer(handler).Serve(ctx, l) }()

	f := startupClient(t, l.Addr().String(), "alice")
	if err := f.WriteMessage(MsgQuery, append([]byte("SELECT 7"), 0)); err != nil {
		t.Fatalf("write query: %v", err)
	}
	if err := f.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var gotRow string
	for {
		mt, payload, err := f.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt == MsgDataRow {
			n := int(binary.BigEndian.Uint32(payload[2:6]))
			gotRow = string(payload[6 : 6+n])
		}
		if mt == MsgReadyForQuery {
			break
		}
	}
	if gotRow != "7" {
		t.Errorf("row = %q, want 7", gotRow)
	}

	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve returned %v, want nil", err)
	}
}

func TestServerStateIsolation(t *testing.T) {
	t.Parallel()
	// Each handler invocation gets its own ConnState; record the statement set
	// size it observes after inserting one entry. It must always be exactly 1.
	sizes := make(chan int, 2)
	handler := func(ctx context.Context, f *Framer, state *ConnState) error {
		state.Statements["s"] = &PreparedStatement{Name: "s"}
		sizes <- len(state.Statements)
		_, _, err := f.ReadMessage() // block until the client disconnects
		return err
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- silentServer(handler).Serve(ctx, l) }()

	c1 := startupClient(t, l.Addr().String(), "u1")
	c2 := startupClient(t, l.Addr().String(), "u2")
	for i := 0; i < 2; i++ {
		if n := <-sizes; n != 1 {
			t.Errorf("statement count = %d, want 1 (state leaked between connections)", n)
		}
	}
	_ = c1.Close()
	_ = c2.Close()
	cancel()
	<-served
}

func TestServerGracefulShutdown(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- silentServer(func(context.Context, *Framer, *ConnState) error { return nil }).Serve(ctx, l)
	}()

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
}
```

## Review

The server is correct when each connection is isolated, the handshake precedes the handler, and shutdown is ordered. Every accepted connection runs in its own goroutine with its own `ConnState`; the isolation test proves that the statement map one handler populates is never visible to another. The startup handshake runs before the handler and a non-3.0 version is rejected with a `FATAL` `ErrorResponse` rather than a silent drop. On context cancel, `Serve` closes the listener, drains active sessions through the `WaitGroup`, bounds the wait by the shutdown timeout, and returns `nil` for the expected post-close accept error.

The mistakes to avoid: sharing one `ConnState` across connections (logical corruption a mutex cannot fix — allocate per connection in `serveConn`); guarding protocol state with the bookkeeping mutex (the mutex is only for the active count); and treating the post-`Close` accept error as a real failure (check `ctx.Done()` first and report `nil`). Wiring SIGINT to shutdown belongs at the call site via `signal.NotifyContext`, keeping the server dependent only on the context.

## Resources

- [PostgreSQL: Protocol Flow](https://www.postgresql.org/docs/current/protocol-flow.html) — the startup-then-query session shape each connection follows.
- [`net` package](https://pkg.go.dev/net) — `net.Listener`, `net.Conn`, `Accept`, and `net.ErrClosed`.
- [`context` package](https://pkg.go.dev/context) — cancellation propagation; pair with `signal.NotifyContext`.
- [`os/signal` — NotifyContext](https://pkg.go.dev/os/signal#NotifyContext) — cancel a context on SIGINT/SIGTERM at the call site.

---

Back to [02-query-and-error-responses.md](02-query-and-error-responses.md) | Next: [04-client-server-round-trip.md](04-client-server-round-trip.md)
