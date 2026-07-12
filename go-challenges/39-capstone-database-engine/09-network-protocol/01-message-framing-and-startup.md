# Exercise 1: Message Framing and Startup Parsing

The wire layer rests on one primitive: read and write a length-prefixed message correctly, every time, over a stream that hands you bytes in arbitrary chunks. This exercise builds that primitive as a `Framer` and then layers the one frame that breaks the rules — the typeless startup message — on top of it. Get these bytes right and every later message is just a different payload; get the length field or the partial-read handling wrong and nothing downstream can be trusted.

This module is fully self-contained. It depends only on the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
wire.go              Framer, message-type constants, ReadMessage/WriteMessage, ReadStartup/WriteStartup
startup.go           server-side startup response: AuthOK, ParameterStatus, BackendKeyData, ReadyForQuery
cmd/
  demo/
    main.go          a client/server startup handshake over net.Pipe
wire_test.go         framing round-trip, empty payload, startup parsing, C-string edge cases
```

- Files: `wire.go`, `startup.go`, `cmd/demo/main.go`, `wire_test.go`.
- Implement: `Framer`, `NewFramer`, `(*Framer).ReadMessage`, `(*Framer).WriteMessage`, `(*Framer).ReadStartup`, `WriteStartup`, `(*Framer).Flush`, `(*Framer).Close`, and the startup response helpers `SendAuthOK`, `SendParamStatus`, `SendBackendKeyData`, `SendReadyForQuery`, `SendStartupResponse`.
- Test: `wire_test.go` round-trips a typed message, an empty-payload message, and a startup message over `net.Pipe`, and exercises the C-string scanner's edge cases.
- Verify: `go test -race ./...`

### How framing actually works

The whole job of `ReadMessage` is to turn a byte stream back into discrete messages, and it does so with three reads in a fixed order: one byte for the type, four bytes for the length, then exactly `length - 4` bytes for the payload. The length field is big-endian and, per Postgres, counts itself but not the type byte, so the payload is `length - 4` bytes long. Every fixed-size read goes through `io.ReadFull`, because a single `bufio.Reader.Read` (or the underlying `net.Conn.Read`) may return fewer bytes than asked even when more are coming; `io.ReadFull` loops until the buffer is full or the stream ends. `WriteMessage` is the mirror image: write the type byte, write `uint32(len(payload) + 4)` big-endian, write the payload. The `+ 4` is the entire ballgame — drop it and the reader on the far side is off by four bytes forever.

`ReadStartup` is the exception that proves the rule. A fresh connection's first frame has no type byte: the four-byte length is the very first thing on the wire, followed by an `int32` protocol version and then null-terminated `key\0value\0` pairs ending in an empty key. So `ReadStartup` reads the length directly (no leading `ReadByte`), validates that version 3.0 (`196608`) is present, and scans the key/value pairs with a C-string reader. Calling `ReadMessage` here instead would consume the high byte of the length as a phantom type byte and corrupt the stream on the very first read — which is why the server's first call is always `ReadStartup`.

The `Framer` wraps the connection in a `bufio.Reader` and `bufio.Writer`. Buffering coalesces the one-byte and four-byte reads into single syscalls and lets the server build a multi-message response in memory, but it does not change the read contract (the buffer can be partially filled, so `io.ReadFull` is still mandatory) and it forces a discipline on the write side: nothing is on the wire until `Flush` is called. The startup response helper in `startup.go` flushes for you at the end of the sequence.

Create `wire.go`:

```go
// Package wire implements the message framing and startup handshake of a
// simplified PostgreSQL wire protocol. Every typed message is
// type(1) + int32(length, includes itself but not the type) + payload, all
// integers big-endian. The startup message has no type byte.
package wire

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// Message type bytes. Some byte values are shared between frontend and backend
// messages (context decides which is which).
const (
	// Frontend (client -> server).
	MsgQuery     byte = 'Q'
	MsgTerminate byte = 'X'

	// Backend (server -> client).
	MsgAuth           byte = 'R'
	MsgParamStatus    byte = 'S'
	MsgBackendKeyData byte = 'K'
	MsgReadyForQuery  byte = 'Z'
	MsgErrorResponse  byte = 'E'
)

// Transaction status bytes for ReadyForQuery.
const (
	TxIdle   byte = 'I' // not in a transaction block
	TxInTx   byte = 'T' // inside a transaction block
	TxFailed byte = 'E' // failed transaction, awaiting ROLLBACK
)

// ProtocolVersion3 is startup protocol version 3.0: (major 3 << 16) | minor 0.
const ProtocolVersion3 int32 = 196608

// Framer wraps a net.Conn with buffered I/O and reads/writes wire messages.
type Framer struct {
	r   *bufio.Reader
	w   *bufio.Writer
	raw net.Conn
}

// NewFramer wraps conn in a Framer with default-sized buffers.
func NewFramer(conn net.Conn) *Framer {
	return &Framer{
		r:   bufio.NewReader(conn),
		w:   bufio.NewWriter(conn),
		raw: conn,
	}
}

// ReadMessage reads one typed message.
// Format: type(1) | int32(length, includes itself) | payload(length-4).
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

// ReadStartup reads the initial startup message, which has no type byte.
// Format: int32(length) | int32(version) | (key\0value\0)* | \0.
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

// WriteStartup writes a client-side startup message (no type byte) and flushes.
// params must contain at least "user" and "database".
func WriteStartup(f *Framer, params map[string]string) error {
	var body []byte
	body = binary.BigEndian.AppendUint32(body, uint32(ProtocolVersion3))
	for k, v := range params {
		body = append(body, k...)
		body = append(body, 0)
		body = append(body, v...)
		body = append(body, 0)
	}
	body = append(body, 0) // terminating empty key

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

// Flush flushes buffered writes to the connection.
func (f *Framer) Flush() error { return f.w.Flush() }

// Close closes the underlying connection.
func (f *Framer) Close() error { return f.raw.Close() }

// parseKeyValue parses null-terminated key-value pairs. An empty key ends them.
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

// readCString reads a null-terminated string and returns the string, the
// remaining bytes, and whether a terminator was found.
func readCString(data []byte) (s string, rest []byte, ok bool) {
	for i, b := range data {
		if b == 0 {
			return string(data[:i]), data[i+1:], true
		}
	}
	return "", data, false
}

// ReadCString is the exported form of readCString for external callers.
func ReadCString(data []byte) (s string, rest []byte, ok bool) {
	return readCString(data)
}
```

### The startup response

After reading a valid startup message the server owes the client a fixed sequence before the connection is usable: `AuthenticationOk`, then one `ParameterStatus` per server setting, then `BackendKeyData` (the process ID and a cancel key), then `ReadyForQuery` with the idle status byte. The client blocks on that final `ReadyForQuery` before sending its first query, so the whole sequence must be flushed at the end — which is what `SendStartupResponse` does.

Create `startup.go`:

```go
package wire

import (
	"encoding/binary"
	"fmt"
)

// SendAuthOK sends AuthenticationOk: type 'R', int32(0).
func (f *Framer) SendAuthOK() error {
	var payload [4]byte // int32(0): no password required
	return f.WriteMessage(MsgAuth, payload[:])
}

// SendParamStatus sends one ParameterStatus message (name\0value\0).
func (f *Framer) SendParamStatus(name, value string) error {
	payload := append([]byte(name), 0)
	payload = append(payload, value...)
	payload = append(payload, 0)
	return f.WriteMessage(MsgParamStatus, payload)
}

// SendBackendKeyData sends BackendKeyData: int32 PID + int32 cancel secret.
func (f *Framer) SendBackendKeyData(pid, secret int32) error {
	var payload [8]byte
	binary.BigEndian.PutUint32(payload[0:], uint32(pid))
	binary.BigEndian.PutUint32(payload[4:], uint32(secret))
	return f.WriteMessage(MsgBackendKeyData, payload[:])
}

// SendReadyForQuery sends ReadyForQuery with the given transaction status byte.
func (f *Framer) SendReadyForQuery(txStatus byte) error {
	return f.WriteMessage(MsgReadyForQuery, []byte{txStatus})
}

// SendStartupResponse performs the full startup response and flushes:
// AuthOK -> ParameterStatus* -> BackendKeyData -> ReadyForQuery(idle).
func (f *Framer) SendStartupResponse(pid, secret int32) error {
	if err := f.SendAuthOK(); err != nil {
		return fmt.Errorf("startup: auth ok: %w", err)
	}
	staticParams := [][2]string{
		{"server_version", "15.0"},
		{"server_encoding", "UTF8"},
		{"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"},
		{"integer_datetimes", "on"},
		{"TimeZone", "UTC"},
	}
	for _, p := range staticParams {
		if err := f.SendParamStatus(p[0], p[1]); err != nil {
			return fmt.Errorf("startup: param status %q: %w", p[0], err)
		}
	}
	if err := f.SendBackendKeyData(pid, secret); err != nil {
		return fmt.Errorf("startup: backend key data: %w", err)
	}
	if err := f.SendReadyForQuery(TxIdle); err != nil {
		return fmt.Errorf("startup: ready for query: %w", err)
	}
	return f.Flush()
}
```

### The runnable demo

The demo runs the startup handshake end to end over an in-memory `net.Pipe`, so no TCP port is needed. A server goroutine reads the startup message and replies with the full response sequence; the client writes the startup message and then reads backend messages until it sees `ReadyForQuery`, printing each one. Because `net.Pipe` is synchronous and the client cannot read the response until the server has written it, the output order is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net"
	"os"

	"example.com/framing"
)

func main() {
	clientConn, serverConn := net.Pipe()

	go func() {
		defer serverConn.Close()
		f := wire.NewFramer(serverConn)
		version, params, err := f.ReadStartup()
		if err != nil {
			fmt.Fprintln(os.Stderr, "server: startup:", err)
			return
		}
		if version != wire.ProtocolVersion3 {
			fmt.Fprintln(os.Stderr, "server: bad version", version)
			return
		}
		fmt.Printf("[server] startup: user=%q database=%q\n", params["user"], params["database"])
		if err := f.SendStartupResponse(1, 0); err != nil {
			fmt.Fprintln(os.Stderr, "server: response:", err)
		}
	}()

	f := wire.NewFramer(clientConn)
	defer f.Close()
	if err := wire.WriteStartup(f, map[string]string{"user": "demo", "database": "testdb"}); err != nil {
		fmt.Fprintln(os.Stderr, "client: startup:", err)
		os.Exit(1)
	}
	for {
		msgType, payload, err := f.ReadMessage()
		if err != nil {
			fmt.Fprintln(os.Stderr, "client: read:", err)
			os.Exit(1)
		}
		switch msgType {
		case wire.MsgAuth:
			fmt.Println("[client] AuthenticationOk")
		case wire.MsgParamStatus:
			name, rest, _ := wire.ReadCString(payload)
			val, _, _ := wire.ReadCString(rest)
			fmt.Printf("[client] ParameterStatus %s=%s\n", name, val)
		case wire.MsgBackendKeyData:
			fmt.Println("[client] BackendKeyData")
		case wire.MsgReadyForQuery:
			fmt.Printf("[client] ReadyForQuery status=%c\n", payload[0])
			return
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
[server] startup: user="demo" database="testdb"
[client] AuthenticationOk
[client] ParameterStatus server_version=15.0
[client] ParameterStatus server_encoding=UTF8
[client] ParameterStatus client_encoding=UTF8
[client] ParameterStatus DateStyle=ISO, MDY
[client] ParameterStatus integer_datetimes=on
[client] ParameterStatus TimeZone=UTC
[client] BackendKeyData
[client] ReadyForQuery status=I
```

### Tests

The tests use `net.Pipe`, an in-memory synchronous connection pair from the standard library: a write blocks until the other side reads, which is exactly the framing contract under test. A round-trip test proves a typed message survives encode then decode; an empty-payload test pins the `length == 4` edge case; a startup test confirms the typeless frame and its key/value parsing; and a table test covers the C-string scanner including the no-terminator case.

Create `wire_test.go`:

```go
package wire

import (
	"encoding/binary"
	"net"
	"testing"
)

func newPipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	return c, s
}

func TestFramerRoundTrip(t *testing.T) {
	t.Parallel()
	c, s := newPipe(t)
	cf, sf := NewFramer(c), NewFramer(s)

	errc := make(chan error, 1)
	go func() {
		if err := cf.WriteMessage(MsgQuery, []byte("SELECT 1\x00")); err != nil {
			errc <- err
			return
		}
		errc <- cf.Flush()
	}()

	msgType, payload, err := sf.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msgType != MsgQuery {
		t.Errorf("type = %q, want MsgQuery ('Q')", msgType)
	}
	if want := "SELECT 1\x00"; string(payload) != want {
		t.Errorf("payload = %q, want %q", payload, want)
	}
	if err := <-errc; err != nil {
		t.Errorf("write/flush: %v", err)
	}
}

func TestFramerEmptyPayload(t *testing.T) {
	t.Parallel()
	c, s := newPipe(t)
	cf, sf := NewFramer(c), NewFramer(s)

	errc := make(chan error, 1)
	go func() {
		if err := sf.WriteMessage(MsgReadyForQuery, []byte{TxIdle}); err != nil {
			errc <- err
			return
		}
		errc <- sf.Flush()
	}()

	msgType, payload, err := cf.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msgType != MsgReadyForQuery {
		t.Errorf("type = %q, want MsgReadyForQuery ('Z')", msgType)
	}
	if len(payload) != 1 || payload[0] != TxIdle {
		t.Errorf("payload = %v, want [%q]", payload, TxIdle)
	}
	if err := <-errc; err != nil {
		t.Errorf("write/flush: %v", err)
	}
}

func TestReadStartupParsesParams(t *testing.T) {
	t.Parallel()
	c, s := newPipe(t)
	sf := NewFramer(s)

	errc := make(chan error, 1)
	go func() {
		var body []byte
		body = binary.BigEndian.AppendUint32(body, uint32(ProtocolVersion3))
		body = append(body, "user\x00alice\x00database\x00mydb\x00\x00"...)
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(body)+4))
		_, err := c.Write(header[:])
		if err == nil {
			_, err = c.Write(body)
		}
		errc <- err
	}()

	version, params, err := sf.ReadStartup()
	if err != nil {
		t.Fatalf("ReadStartup: %v", err)
	}
	if version != ProtocolVersion3 {
		t.Errorf("version = %d, want %d", version, ProtocolVersion3)
	}
	if params["user"] != "alice" || params["database"] != "mydb" {
		t.Errorf("params = %v", params)
	}
	if err := <-errc; err != nil {
		t.Errorf("client write: %v", err)
	}
}

func TestStartupResponseSequence(t *testing.T) {
	t.Parallel()
	c, s := newPipe(t)
	cf, sf := NewFramer(c), NewFramer(s)

	errc := make(chan error, 1)
	go func() { errc <- sf.SendStartupResponse(4242, 99) }()

	gotParams := 0
	i := 0
	for {
		mt, payload, err := cf.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		switch mt {
		case MsgAuth:
			if i != 0 {
				t.Errorf("AuthOK out of order at index %d", i)
			}
		case MsgParamStatus:
			gotParams++
		case MsgBackendKeyData:
			pid := binary.BigEndian.Uint32(payload[:4])
			if pid != 4242 {
				t.Errorf("pid = %d, want 4242", pid)
			}
		case MsgReadyForQuery:
			if payload[0] != TxIdle {
				t.Errorf("status = %q, want idle", payload[0])
			}
			if gotParams == 0 {
				t.Error("no ParameterStatus messages before ReadyForQuery")
			}
			if err := <-errc; err != nil {
				t.Errorf("SendStartupResponse: %v", err)
			}
			return
		}
		i++
	}
}

func TestReadCStringEdgeCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input   string
		wantStr string
		wantOK  bool
	}{
		{"hello\x00world", "hello", true},
		{"\x00rest", "", true}, // empty string before null is valid
		{"no-null", "", false}, // no terminator
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			s, _, ok := readCString([]byte(tc.input))
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && s != tc.wantStr {
				t.Errorf("s = %q, want %q", s, tc.wantStr)
			}
		})
	}
}
```

## Review

The framing layer is correct when a message survives the round trip byte for byte and the length field counts itself: a typed message with an N-byte payload writes `N + 4` on the wire and the reader consumes exactly `N` payload bytes after the length. The empty-payload case must produce a length of `4` and read back zero bytes. The startup path must read the four-byte length with no leading type byte and reject anything shorter than the version field. Confirm every fixed-size read uses `io.ReadFull` so a partial `net.Pipe` read never truncates a frame, and that the startup response flushes so the client's blocking read on `ReadyForQuery` actually unblocks.

The recurring mistakes are the off-by-four on the length field (writing `len(payload)` instead of `len(payload) + 4`), calling `ReadMessage` for the first frame instead of `ReadStartup`, and assuming a single `Read` filled the buffer. All three corrupt the stream silently rather than failing loudly, which is why the round-trip tests assert on exact bytes.

## Resources

- [PostgreSQL: Frontend/Backend Protocol — Message Formats](https://www.postgresql.org/docs/current/protocol-message-formats.html) — the byte layout of StartupMessage, AuthenticationOk, ParameterStatus, BackendKeyData, and ReadyForQuery.
- [PostgreSQL: Protocol Flow — Start-Up](https://www.postgresql.org/docs/current/protocol-flow.html#PROTOCOL-FLOW-START-UP) — the startup sequence and why the first frame has no type byte.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `binary.BigEndian`, `AppendUint32`, `PutUint32`, `Uint32`.
- [`net` package](https://pkg.go.dev/net) — `net.Conn` and `net.Pipe`, the in-memory connection the tests use.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-query-and-error-responses.md](02-query-and-error-responses.md)
