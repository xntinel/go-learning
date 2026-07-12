# Exercise 4: End-to-End Simple Query Round-Trip

The individual messages only matter if they compose into a working conversation. This exercise builds a compact wire package — framing, the startup handshake on both sides, and the simple-query result messages — and then drives a complete client/server exchange over an in-memory `net.Pipe`: the client sends a startup message and a `Query`, the server replies with a row set and `ReadyForQuery`, and both halves run in one process with no TCP port. Seeing the whole simple-query state machine resolve end to end is the point; the demo is the deliverable.

This module is fully self-contained. It depends only on the standard library and ships its own demo and tests, including a duplicated copy of the framing scaffolding so it builds with no dependency on any other exercise.

## What you'll build

```text
wire.go              Framer, framing, ReadStartup/WriteStartup, message-type constants
startup.go           server-side startup response sequence
simple.go            ColumnDesc, SendRowDescription/SendDataRow/SendCmdComplete, CommandTag
cmd/
  demo/
    main.go          a full client + server simple-query exchange over net.Pipe
wire_test.go         an end-to-end round-trip assertion plus the CommandTag table and example
```

- Files: `wire.go`, `startup.go`, `simple.go`, `cmd/demo/main.go`, `wire_test.go`.
- Implement: the client side (`WriteStartup`, sending a `Query`) and the server side (`ReadStartup`, `SendStartupResponse`, the result messages) glued into one conversation.
- Test: `wire_test.go` runs a client and server over `net.Pipe`, sends a `SELECT`, and asserts the three rows and the `CommandComplete` tag come back in order, terminated by `ReadyForQuery`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/09-network-protocol/04-client-server-round-trip/cmd/demo && cd go-solutions/39-capstone-database-engine/09-network-protocol/04-client-server-round-trip
```

### The simple-query conversation, end to end

The simple-query protocol is one round trip and a fixed message order, and this exercise exists to make that order concrete. The client opens with a typeless startup message; the server replies with the handshake sequence (`AuthenticationOk`, `ParameterStatus*`, `BackendKeyData`, `ReadyForQuery`) and then waits. Crucially, the client does nothing until it sees that first `ReadyForQuery` — it is the synchronization point. Only then does the client send a `Query` (type `'Q'`) carrying a null-terminated SQL string. The server answers with one result sequence — `RowDescription`, then a `DataRow` per row, then `CommandComplete`, then `ReadyForQuery` again — and returns to the ready state.

Two disciplines make this work over a real stream, and `net.Pipe` enforces both because it is unbuffered and synchronous. First, every response sequence must be flushed: the server builds `RowDescription` + `DataRow*` + `CommandComplete` + `ReadyForQuery` in its `bufio.Writer` and a single `Flush` puts them on the wire; forget it and the client blocks forever on a read while the bytes sit in the buffer. Second, the client reads in a loop and dispatches on the type byte rather than assuming an order, because it does not control how the kernel chunks the bytes — it reads framed messages until it sees the terminating `ReadyForQuery`. The `net.Pipe` round trip in the test exercises exactly this: a write on one side blocks until the other side reads, so a missing flush or a wrong length deadlocks or corrupts immediately rather than passing by luck.

Create `wire.go`:

```go
// Package wire implements the framing, startup handshake, and simple-query
// result messages of a simplified PostgreSQL wire protocol, enough to run a
// complete client/server simple-query exchange. Every typed message is
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
	MsgRowDescription byte = 'T'
	MsgDataRow        byte = 'D'
	MsgCmdComplete    byte = 'C'
	MsgErrorResponse  byte = 'E'
)

// Transaction status bytes.
const (
	TxIdle byte = 'I'
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

// SendReadyForQuery sends ReadyForQuery with the given transaction status byte.
func (f *Framer) SendReadyForQuery(txStatus byte) error {
	return f.WriteMessage(MsgReadyForQuery, []byte{txStatus})
}

// SendStartupResponse performs the full startup response and flushes:
// AuthOK -> ParameterStatus* -> BackendKeyData -> ReadyForQuery(idle).
func (f *Framer) SendStartupResponse(pid, secret int32) error {
	var authOK [4]byte
	if err := f.WriteMessage(MsgAuth, authOK[:]); err != nil {
		return fmt.Errorf("startup: auth ok: %w", err)
	}
	for _, p := range [][2]string{
		{"server_version", "15.0"},
		{"server_encoding", "UTF8"},
		{"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"},
		{"integer_datetimes", "on"},
		{"TimeZone", "UTC"},
	} {
		payload := append([]byte(p[0]), 0)
		payload = append(payload, p[1]...)
		payload = append(payload, 0)
		if err := f.WriteMessage(MsgParamStatus, payload); err != nil {
			return fmt.Errorf("startup: param status %q: %w", p[0], err)
		}
	}
	var key [8]byte
	binary.BigEndian.PutUint32(key[0:], uint32(pid))
	binary.BigEndian.PutUint32(key[4:], uint32(secret))
	if err := f.WriteMessage(MsgBackendKeyData, key[:]); err != nil {
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
		payload = binary.BigEndian.AppendUint32(payload, 0)
		payload = binary.BigEndian.AppendUint16(payload, 0)
		payload = binary.BigEndian.AppendUint32(payload, col.TypeOID)
		payload = binary.BigEndian.AppendUint16(payload, 0xFFFF)
		payload = binary.BigEndian.AppendUint32(payload, 0xFFFFFFFF)
		payload = binary.BigEndian.AppendUint16(payload, 0)
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

### The runnable demo

The demo wires a server goroutine and a client on the two ends of a `net.Pipe`. The server performs the startup handshake and answers a single `SELECT` with a three-row result; the client drives the startup handshake, sends the query, and prints every message it receives. Because the pipe is synchronous, the printed order is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"

	"example.com/roundtrip"
)

func main() {
	clientConn, serverConn := net.Pipe()

	go func() {
		defer serverConn.Close()
		if err := serveOne(serverConn); err != nil {
			fmt.Fprintln(os.Stderr, "server:", err)
		}
	}()

	if err := runClient(clientConn); err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		os.Exit(1)
	}
}

func serveOne(conn net.Conn) error {
	f := wire.NewFramer(conn)
	version, params, err := f.ReadStartup()
	if err != nil {
		return fmt.Errorf("startup read: %w", err)
	}
	if version != wire.ProtocolVersion3 {
		return fmt.Errorf("unexpected protocol version %d", version)
	}
	fmt.Printf("[server] startup: user=%q database=%q\n", params["user"], params["database"])
	if err := f.SendStartupResponse(1, 0); err != nil {
		return fmt.Errorf("startup response: %w", err)
	}

	msgType, payload, err := f.ReadMessage()
	if err != nil {
		return fmt.Errorf("read query: %w", err)
	}
	if msgType != wire.MsgQuery {
		return fmt.Errorf("expected Query, got %q", msgType)
	}
	query, _, _ := wire.ReadCString(payload)
	fmt.Printf("[server] received query: %q\n", query)

	if err := f.SendRowDescription([]wire.ColumnDesc{{Name: "n", TypeOID: wire.OIDInt4}}); err != nil {
		return err
	}
	for _, v := range []string{"1", "2", "3"} {
		if err := f.SendDataRow([]*string{wire.StringPtr(v)}); err != nil {
			return err
		}
	}
	if err := f.SendCmdComplete(wire.CommandTag("SELECT", 3)); err != nil {
		return err
	}
	if err := f.SendReadyForQuery(wire.TxIdle); err != nil {
		return err
	}
	return f.Flush()
}

func runClient(conn net.Conn) error {
	defer conn.Close()
	f := wire.NewFramer(conn)

	if err := wire.WriteStartup(f, map[string]string{"user": "demo", "database": "testdb"}); err != nil {
		return fmt.Errorf("startup: %w", err)
	}

	for {
		msgType, payload, err := f.ReadMessage()
		if err != nil {
			return fmt.Errorf("read startup response: %w", err)
		}
		switch msgType {
		case wire.MsgAuth:
			fmt.Println("[client] AuthenticationOk")
		case wire.MsgParamStatus:
			name, _, _ := wire.ReadCString(payload)
			fmt.Printf("[client] ParameterStatus: %q\n", name)
		case wire.MsgBackendKeyData:
			fmt.Println("[client] BackendKeyData received")
		case wire.MsgReadyForQuery:
			fmt.Printf("[client] ReadyForQuery txStatus=%c\n", payload[0])
			goto query
		}
	}

query:
	if err := f.WriteMessage(wire.MsgQuery, append([]byte("SELECT n FROM t"), 0)); err != nil {
		return err
	}
	if err := f.Flush(); err != nil {
		return err
	}

	rows := 0
	for {
		msgType, payload, err := f.ReadMessage()
		if err != nil {
			return fmt.Errorf("read query response: %w", err)
		}
		switch msgType {
		case wire.MsgRowDescription:
			fmt.Println("[client] RowDescription received")
		case wire.MsgDataRow:
			valLen := int(binary.BigEndian.Uint32(payload[2:6]))
			fmt.Printf("[client] DataRow: n=%s\n", payload[6:6+valLen])
			rows++
		case wire.MsgCmdComplete:
			tag, _, _ := wire.ReadCString(payload)
			fmt.Printf("[client] CommandComplete: %q\n", tag)
		case wire.MsgReadyForQuery:
			fmt.Printf("[client] ReadyForQuery (received %d rows)\n", rows)
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
[server] startup: user="demo" database="testdb"
[client] AuthenticationOk
[client] ParameterStatus: "server_version"
[client] ParameterStatus: "server_encoding"
[client] ParameterStatus: "client_encoding"
[client] ParameterStatus: "DateStyle"
[client] ParameterStatus: "integer_datetimes"
[client] ParameterStatus: "TimeZone"
[client] BackendKeyData received
[client] ReadyForQuery txStatus=I
[server] received query: "SELECT n FROM t"
[client] RowDescription received
[client] DataRow: n=1
[client] DataRow: n=2
[client] DataRow: n=3
[client] CommandComplete: "SELECT 3"
[client] ReadyForQuery (received 3 rows)
```

### Tests

The end-to-end test runs the same conversation in-process and asserts on it: a server goroutine and a client over `net.Pipe`, a `SELECT`, and a check that exactly three rows and the `"SELECT 3"` tag arrive before the terminating `ReadyForQuery`. The `CommandTag` table and example pin the tag strings clients parse.

Create `wire_test.go`:

```go
package wire

import (
	"encoding/binary"
	"fmt"
	"net"
	"testing"
)

func TestEndToEndSimpleQuery(t *testing.T) {
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

	srvErr := make(chan error, 1)
	go func() {
		f := NewFramer(serverConn)
		if _, _, err := f.ReadStartup(); err != nil {
			srvErr <- err
			return
		}
		if err := f.SendStartupResponse(1, 0); err != nil {
			srvErr <- err
			return
		}
		if _, _, err := f.ReadMessage(); err != nil {
			srvErr <- err
			return
		}
		_ = f.SendRowDescription([]ColumnDesc{{Name: "n", TypeOID: OIDInt4}})
		for _, v := range []string{"1", "2", "3"} {
			_ = f.SendDataRow([]*string{StringPtr(v)})
		}
		_ = f.SendCmdComplete(CommandTag("SELECT", 3))
		_ = f.SendReadyForQuery(TxIdle)
		srvErr <- f.Flush()
	}()

	f := NewFramer(clientConn)
	if err := WriteStartup(f, map[string]string{"user": "u", "database": "d"}); err != nil {
		t.Fatalf("WriteStartup: %v", err)
	}
	for {
		mt, _, err := f.ReadMessage()
		if err != nil {
			t.Fatalf("read handshake: %v", err)
		}
		if mt == MsgReadyForQuery {
			break
		}
	}
	if err := f.WriteMessage(MsgQuery, append([]byte("SELECT n FROM t"), 0)); err != nil {
		t.Fatalf("write query: %v", err)
	}
	if err := f.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var rows []string
	var tag string
	for {
		mt, payload, err := f.ReadMessage()
		if err != nil {
			t.Fatalf("read result: %v", err)
		}
		switch mt {
		case MsgDataRow:
			n := int(binary.BigEndian.Uint32(payload[2:6]))
			rows = append(rows, string(payload[6:6+n]))
		case MsgCmdComplete:
			tag, _, _ = ReadCString(payload)
		case MsgReadyForQuery:
			if len(rows) != 3 || rows[0] != "1" || rows[2] != "3" {
				t.Errorf("rows = %v, want [1 2 3]", rows)
			}
			if tag != "SELECT 3" {
				t.Errorf("tag = %q, want %q", tag, "SELECT 3")
			}
			if err := <-srvErr; err != nil {
				t.Errorf("server: %v", err)
			}
			return
		}
	}
}

func TestCommandTagTable(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"SELECT 3":   CommandTag("SELECT", 3),
		"INSERT 0 1": CommandTag("INSERT", 1),
		"UPDATE 5":   CommandTag("UPDATE", 5),
		"DELETE 2":   CommandTag("DELETE", 2),
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func ExampleCommandTag() {
	fmt.Println(CommandTag("SELECT", 5))
	fmt.Println(CommandTag("INSERT", 1))
	// Output:
	// SELECT 5
	// INSERT 0 1
}
```

## Review

The exercise is correct when the whole simple-query conversation resolves in order with no manual byte counting on the happy path: the client waits for the first `ReadyForQuery` before sending its `Query`, the server flushes the full result sequence so the client's reads unblock, and the client loops on the type byte until the terminating `ReadyForQuery`. The test proves exactly three rows and the `"SELECT 3"` tag arrive, and that the server side returns no error.

The mistakes this exercise is built to surface are the two that deadlock or corrupt a real connection: forgetting the final `Flush` (the client hangs on a read while the result sits in the server's buffer) and sending a query before the first `ReadyForQuery` (a well-behaved server has not signaled readiness yet). Both pass unnoticed in a buffered, lucky run and fail deterministically over the synchronous `net.Pipe`.

## Resources

- [PostgreSQL: Protocol Flow — Simple Query](https://www.postgresql.org/docs/current/protocol-flow.html#PROTOCOL-FLOW-SIMPLE-QUERY) — the RowDescription / DataRow / CommandComplete / ReadyForQuery sequence.
- [PostgreSQL: Message Formats](https://www.postgresql.org/docs/current/protocol-message-formats.html) — the byte layout of Query, RowDescription, DataRow, and CommandComplete.
- [`net.Pipe`](https://pkg.go.dev/net#Pipe) — the in-memory synchronous connection the demo and test use.
- [`bufio` package](https://pkg.go.dev/bufio) — why a buffered writer requires an explicit `Flush`.

---

Back to [03-concurrent-tcp-server.md](03-concurrent-tcp-server.md) | Next: [05-extended-query-flow.md](05-extended-query-flow.md)
