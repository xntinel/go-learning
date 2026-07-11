# Exercise 2: Query Results and Error Responses

A query that succeeds and a query that fails both have to come back as precise bytes. This exercise builds the result-encoding side of the protocol — `RowDescription`, `DataRow` (with correct NULL handling), and `CommandComplete` — plus the structured `ErrorResponse` that carries a SQLSTATE code clients parse, and the two extended-protocol decoders (`Parse` and `Bind`) whose trailing result-format codes are the classic place a decoder silently misaligns the stream.

This module is fully self-contained. It depends only on the standard library and ships its own demo and tests, including a duplicated copy of the framing scaffolding so it builds with no dependency on any other exercise.

## What you'll build

```text
wire.go              Framer scaffolding (NewFramer, ReadMessage, WriteMessage, Flush), message-type constants
error.go             WireError, SQLSTATE constants, ToWireError, SendErrorResponse
simple.go            ColumnDesc, type OIDs, SendRowDescription, SendDataRow, SendCmdComplete, CommandTag
extended.go          PreparedStatement, Portal, ConnState, DecodeParse, DecodeBind, completion messages
cmd/
  demo/
    main.go          encode a result set + an error response, decode them over net.Pipe
wire_test.go         error/SQLSTATE, CommandTag table, RowDescription/DataRow, Parse/Bind decoding
```

- Files: `wire.go`, `error.go`, `simple.go`, `extended.go`, `cmd/demo/main.go`, `wire_test.go`.
- Implement: `WireError`, `ToWireError`, `(*Framer).SendErrorResponse`; `SendRowDescription`, `SendDataRow`, `SendCmdComplete`, `CommandTag`; `DecodeParse`, `DecodeBind`, `ConnState`.
- Test: `wire_test.go` checks the SQLSTATE survives in an `ErrorResponse`, the command tags match Postgres, a NULL column encodes as length `-1`, and `DecodeBind` both consumes the trailing result-format codes and rejects a truncated payload.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p queryproto/cmd/demo && cd queryproto
go mod init example.com/queryproto
```

### Encoding a result set

Three messages carry a result. `RowDescription` (type `'T'`) names the columns: an `int16` count, then per column a null-terminated name, a table OID, a column attribute number, the type OID, the type size, a type modifier, and a format code. This lesson sends text format (format code `0`) for every column, which is the only format a client requires. `DataRow` (type `'D'`) carries one row: an `int16` value count, then per value an `int32` length followed by that many bytes. The length `-1` (`0xFFFFFFFF`) is the SQL NULL sentinel and is followed by zero bytes; this is the single most important detail in the message, because a zero-length value is the empty string, not NULL, and clients tell them apart. Representing a value as `*string` makes the distinction structural: a `nil` pointer is NULL, a pointer to `""` is the empty string. `CommandComplete` (type `'C'`) carries the tag — `"SELECT 3"`, `"INSERT 0 1"`, `"UPDATE 5"` — as a single null-terminated string. INSERT's tag has a `0` in the second field, a legacy slot for the inserted-row OID.

### Encoding an error

`ErrorResponse` (type `'E'`) is a sequence of field-tagged strings ending in a zero byte: each field is a one-byte code followed by a null-terminated value, and an empty value is omitted. The codes that matter are `'S'` severity, `'V'` non-localized severity, `'C'` SQLSTATE, `'M'` message, and optionally `'D'` detail, `'H'` hint, `'P'` position. The SQLSTATE is a five-character code from the SQL standard; clients like `pgx` map it to a typed error, so a wrong code makes the client misclassify the failure (a unique-violation reported as an internal error, say). Modeling the error as a `WireError` struct keeps the code with the message, and `ToWireError` funnels any stray `error` into a `WireError` carrying `XX000` (internal error) so the handler always has a SQLSTATE to send.

### Decoding Parse and Bind, and why the trailing codes are not optional

The extended protocol's `Parse` names a query and declares parameter type OIDs; `Bind` supplies concrete parameter values for a named statement. `DecodeBind` is where decoders go wrong. Its payload is: portal name, statement name, an `int16` count of parameter format codes followed by those codes, an `int16` count of parameters followed by each parameter's `int32` length and bytes (or `-1` for NULL), and then — the part people drop — an `int16` count of result format codes followed by those codes. A decoder that stops after the parameter values leaves the result-format bytes unread in the buffer. Over a real connection those bytes are still in the stream, so the next `ReadMessage` reads them as a phantom type byte and length, and the connection is corrupt from that point on. The decoder here consumes the result-format codes fully and treats any leftover bytes as a protocol error, so the frame is accounted for to the last byte. Every length check returns a wrapped `ErrShortMessage`, so a truncated payload is an `errors.Is`-classifiable error rather than a slice-bounds panic.

Create `wire.go` (the framing scaffolding this module needs, duplicated so it stands alone):

```go
// Package wire implements query-result and error encoding plus the Parse/Bind
// decoders of a simplified PostgreSQL wire protocol. Every typed message is
// type(1) + int32(length, includes itself) + payload, all integers big-endian.
package wire

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// Message type bytes (backend unless noted).
const (
	MsgRowDescription byte = 'T'
	MsgDataRow        byte = 'D'
	MsgCmdComplete    byte = 'C'
	MsgErrorResponse  byte = 'E'
	MsgEmptyQueryResp byte = 'I'
	MsgParseComplete  byte = '1'
	MsgBindComplete   byte = '2'
	MsgParamDesc      byte = 't'
	MsgNoData         byte = 'n'
)

// Transaction status bytes for ReadyForQuery.
const (
	TxIdle   byte = 'I'
	TxInTx   byte = 'T'
	TxFailed byte = 'E'
)

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

// Flush flushes buffered writes.
func (f *Framer) Flush() error { return f.w.Flush() }

// Close closes the underlying connection.
func (f *Framer) Close() error { return f.raw.Close() }

// readCString reads a null-terminated string and returns the remainder.
func readCString(data []byte) (s string, rest []byte, ok bool) {
	for i, b := range data {
		if b == 0 {
			return string(data[:i]), data[i+1:], true
		}
	}
	return "", data, false
}

// ReadCString is the exported form for external callers (e.g. cmd/demo).
func ReadCString(data []byte) (s string, rest []byte, ok bool) {
	return readCString(data)
}
```

Create `error.go`:

```go
package wire

import (
	"errors"
	"fmt"
)

// SQLSTATE codes (five-character SQL-standard strings).
const (
	SQLStateSyntaxError         = "42601"
	SQLStateUndefinedTable      = "42P01"
	SQLStateUndefinedColumn     = "42703"
	SQLStateUniqueViolation     = "23505"
	SQLStateFeatureNotSupported = "0A000"
	SQLStateInternalError       = "XX000"
)

// WireError is a structured protocol error that serializes into an
// ErrorResponse. Its Code must be a valid SQLSTATE string.
type WireError struct {
	Severity string // "ERROR", "FATAL", "WARNING", ...
	Code     string // SQLSTATE five-character code
	Message  string // primary human-readable message
	Detail   string // optional extra context
	Hint     string // optional hint
	Position string // optional 1-based cursor position
}

// Error implements the error interface.
func (e *WireError) Error() string { return e.Message }

// Common sentinel errors. Wrap with fmt.Errorf("%w: ...", ...) and match with
// errors.As to keep the SQLSTATE while adding context.
var (
	ErrSyntaxError     = &WireError{Severity: "ERROR", Code: SQLStateSyntaxError, Message: "syntax error"}
	ErrUndefinedTable  = &WireError{Severity: "ERROR", Code: SQLStateUndefinedTable, Message: "relation does not exist"}
	ErrUniqueViolation = &WireError{Severity: "ERROR", Code: SQLStateUniqueViolation, Message: "duplicate key value violates unique constraint"}
)

// ToWireError converts any error to a *WireError. If err already is (or wraps)
// one it is returned directly; otherwise an internal error (XX000) is returned.
func ToWireError(err error) *WireError {
	var we *WireError
	if errors.As(err, &we) {
		return we
	}
	return &WireError{Severity: "ERROR", Code: SQLStateInternalError, Message: err.Error()}
}

// SendErrorResponse encodes and sends an ErrorResponse and flushes. Each field
// is a one-byte code + null-terminated value; a final zero byte ends the list.
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
	appendField('V', we.Severity) // non-localized severity
	appendField('C', we.Code)
	appendField('M', we.Message)
	appendField('D', we.Detail)
	appendField('H', we.Hint)
	appendField('P', we.Position)
	payload = append(payload, 0) // message terminator
	if err := f.WriteMessage(MsgErrorResponse, payload); err != nil {
		return fmt.Errorf("error response: %w", err)
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
	OIDText   uint32 = 25
	OIDInt4   uint32 = 23
	OIDInt8   uint32 = 20
	OIDFloat8 uint32 = 701
	OIDBool   uint32 = 16
)

// SendRowDescription sends a RowDescription (text format for every column).
func (f *Framer) SendRowDescription(cols []ColumnDesc) error {
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(cols)))
	for _, col := range cols {
		payload = append(payload, col.Name...)
		payload = append(payload, 0)
		payload = binary.BigEndian.AppendUint32(payload, 0)                                // table OID
		payload = binary.BigEndian.AppendUint16(payload, 0)                                // column attr number
		payload = binary.BigEndian.AppendUint32(payload, col.TypeOID)                      // type OID
		payload = binary.BigEndian.AppendUint16(payload, uint16(colTypeSize(col.TypeOID))) // type size
		payload = binary.BigEndian.AppendUint32(payload, 0xFFFFFFFF)                       // type modifier (-1)
		payload = binary.BigEndian.AppendUint16(payload, 0)                                // format code (text)
	}
	return f.WriteMessage(MsgRowDescription, payload)
}

// colTypeSize returns the fixed byte size of a type, or 0xFFFF for var-length.
func colTypeSize(oid uint32) uint16 {
	switch oid {
	case OIDInt4:
		return 4
	case OIDInt8, OIDFloat8:
		return 8
	case OIDBool:
		return 1
	default:
		return 0xFFFF
	}
}

// SendDataRow sends one DataRow. A nil value encodes as SQL NULL (length -1).
func (f *Framer) SendDataRow(values []*string) error {
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(values)))
	for _, v := range values {
		if v == nil {
			payload = binary.BigEndian.AppendUint32(payload, 0xFFFFFFFF) // NULL
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

// SendEmptyQueryResponse sends EmptyQueryResponse for an empty query string.
func (f *Framer) SendEmptyQueryResponse() error {
	return f.WriteMessage(MsgEmptyQueryResp, nil)
}

// StringPtr returns a pointer to a copy of s, for building DataRow values inline.
func StringPtr(s string) *string { return &s }

// CommandTag builds the CommandComplete tag for a SQL command verb.
func CommandTag(cmd string, rowsAffected int) string {
	switch strings.ToUpper(cmd) {
	case "SELECT":
		return fmt.Sprintf("SELECT %d", rowsAffected)
	case "INSERT":
		return fmt.Sprintf("INSERT 0 %d", rowsAffected) // second field is legacy OID
	case "UPDATE":
		return fmt.Sprintf("UPDATE %d", rowsAffected)
	case "DELETE":
		return fmt.Sprintf("DELETE %d", rowsAffected)
	default:
		return cmd
	}
}
```

Create `extended.go`:

```go
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Extended-protocol state errors. Wrap with %w so callers can errors.Is them.
var (
	ErrNoSuchStatement = errors.New("wire: prepared statement does not exist")
	ErrShortMessage    = errors.New("wire: message payload truncated")
)

// PreparedStatement is the result of a Parse: a named query with parameter OIDs.
type PreparedStatement struct {
	Name       string
	Query      string
	ParamTypes []uint32
}

// Portal is the result of a Bind: a statement with concrete parameter values.
type Portal struct {
	Name          string
	Statement     *PreparedStatement
	Params        [][]byte // nil element = SQL NULL
	ResultFormats []int16  // 0 = text, 1 = binary
}

// ConnState holds one connection's prepared statements, portals, and tx status.
// It is never shared between connections.
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

// SendParseComplete sends ParseComplete (type '1', empty payload).
func (f *Framer) SendParseComplete() error { return f.WriteMessage(MsgParseComplete, nil) }

// SendBindComplete sends BindComplete (type '2', empty payload).
func (f *Framer) SendBindComplete() error { return f.WriteMessage(MsgBindComplete, nil) }

// SendNoData sends NoData (type 'n').
func (f *Framer) SendNoData() error { return f.WriteMessage(MsgNoData, nil) }

// SendParamDescription sends ParameterDescription with the given type OIDs.
func (f *Framer) SendParamDescription(oids []uint32) error {
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(oids)))
	for _, oid := range oids {
		payload = binary.BigEndian.AppendUint32(payload, oid)
	}
	return f.WriteMessage(MsgParamDesc, payload)
}

// DecodeParse decodes a Parse payload: name\0 query\0 int16(n) [int32(oid)]*.
func DecodeParse(payload []byte) (*PreparedStatement, error) {
	name, rest, ok := readCString(payload)
	if !ok {
		return nil, fmt.Errorf("wire: parse name: %w", ErrShortMessage)
	}
	query, rest2, ok := readCString(rest)
	if !ok {
		return nil, fmt.Errorf("wire: parse query: %w", ErrShortMessage)
	}
	if len(rest2) < 2 {
		return nil, fmt.Errorf("wire: parse count: %w", ErrShortMessage)
	}
	n := int(binary.BigEndian.Uint16(rest2[:2]))
	rest2 = rest2[2:]
	if len(rest2) < n*4 {
		return nil, fmt.Errorf("wire: parse oids: %w", ErrShortMessage)
	}
	oids := make([]uint32, n)
	for i := range oids {
		oids[i] = binary.BigEndian.Uint32(rest2[i*4:])
	}
	return &PreparedStatement{Name: name, Query: query, ParamTypes: oids}, nil
}

// DecodeBind decodes a Bind payload into a Portal. Text-format parameters only.
// Layout: portal\0 stmt\0 int16(nFmt) fmts int16(nParam) [int32(len)+data | -1]*
//
//	int16(nResultFmt) resultFmts.
//
// The trailing result-format codes are not optional: real clients send them,
// and a decoder that stops after the parameter values leaves them unread,
// misaligning the next message. This decoder consumes them and rejects leftovers.
func DecodeBind(payload []byte, state *ConnState) (*Portal, error) {
	portalName, rest, ok := readCString(payload)
	if !ok {
		return nil, fmt.Errorf("wire: bind portal: %w", ErrShortMessage)
	}
	stmtName, rest, ok := readCString(rest)
	if !ok {
		return nil, fmt.Errorf("wire: bind statement: %w", ErrShortMessage)
	}
	stmt, exists := state.Statements[stmtName]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrNoSuchStatement, stmtName)
	}
	// Parameter format codes.
	if len(rest) < 2 {
		return nil, fmt.Errorf("wire: bind param format count: %w", ErrShortMessage)
	}
	numFormats := int(binary.BigEndian.Uint16(rest[:2]))
	if len(rest) < 2+numFormats*2 {
		return nil, fmt.Errorf("wire: bind param formats: %w", ErrShortMessage)
	}
	rest = rest[2+numFormats*2:]
	// Parameter values.
	if len(rest) < 2 {
		return nil, fmt.Errorf("wire: bind param count: %w", ErrShortMessage)
	}
	numParams := int(binary.BigEndian.Uint16(rest[:2]))
	rest = rest[2:]
	params := make([][]byte, numParams)
	for i := range params {
		if len(rest) < 4 {
			return nil, fmt.Errorf("wire: bind param %d len: %w", i, ErrShortMessage)
		}
		length := int32(binary.BigEndian.Uint32(rest[:4]))
		rest = rest[4:]
		if length == -1 {
			params[i] = nil // SQL NULL
			continue
		}
		if int(length) > len(rest) {
			return nil, fmt.Errorf("wire: bind param %d data: %w", i, ErrShortMessage)
		}
		params[i] = rest[:length]
		rest = rest[length:]
	}
	// Result format codes: consuming these keeps the stream aligned.
	if len(rest) < 2 {
		return nil, fmt.Errorf("wire: bind result format count: %w", ErrShortMessage)
	}
	numResultFormats := int(binary.BigEndian.Uint16(rest[:2]))
	if len(rest) < 2+numResultFormats*2 {
		return nil, fmt.Errorf("wire: bind result formats: %w", ErrShortMessage)
	}
	rest = rest[2:]
	var resultFormats []int16
	if numResultFormats > 0 {
		resultFormats = make([]int16, numResultFormats)
		for i := range resultFormats {
			resultFormats[i] = int16(binary.BigEndian.Uint16(rest[i*2:]))
		}
	}
	rest = rest[numResultFormats*2:]
	if len(rest) != 0 {
		return nil, fmt.Errorf("wire: bind: %d trailing bytes", len(rest))
	}
	return &Portal{Name: portalName, Statement: stmt, Params: params, ResultFormats: resultFormats}, nil
}
```

### The runnable demo

The demo wires a server and client over `net.Pipe`. The server sends a two-column, two-row result (the second row has a NULL name) followed by a `CommandComplete`, then a separate `ErrorResponse`. The client decodes each message and prints it, so the NULL column and the SQLSTATE are both visible in the output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"

	"example.com/queryproto"
)

func main() {
	clientConn, serverConn := net.Pipe()

	go func() {
		defer serverConn.Close()
		sf := wire.NewFramer(serverConn)
		_ = sf.SendRowDescription([]wire.ColumnDesc{
			{Name: "id", TypeOID: wire.OIDInt4},
			{Name: "name", TypeOID: wire.OIDText},
		})
		_ = sf.SendDataRow([]*string{wire.StringPtr("42"), wire.StringPtr("Alice")})
		_ = sf.SendDataRow([]*string{wire.StringPtr("99"), nil}) // NULL name
		_ = sf.SendCmdComplete(wire.CommandTag("SELECT", 2))
		_ = sf.SendErrorResponse(&wire.WireError{
			Severity: "ERROR",
			Code:     wire.SQLStateUndefinedTable,
			Message:  `relation "ghost" does not exist`,
		})
		_ = sf.Flush()
	}()

	cf := wire.NewFramer(clientConn)
	defer cf.Close()
	for {
		mt, payload, err := cf.ReadMessage()
		if err != nil {
			fmt.Fprintln(os.Stderr, "client: read:", err)
			os.Exit(1)
		}
		switch mt {
		case wire.MsgRowDescription:
			n := binary.BigEndian.Uint16(payload[:2])
			fmt.Printf("[client] RowDescription: %d columns\n", n)
		case wire.MsgDataRow:
			fmt.Printf("[client] DataRow: %s\n", formatRow(payload))
		case wire.MsgCmdComplete:
			tag, _, _ := wire.ReadCString(payload)
			fmt.Printf("[client] CommandComplete: %q\n", tag)
		case wire.MsgErrorResponse:
			fmt.Printf("[client] ErrorResponse: %s\n", formatError(payload))
			return
		}
	}
}

func formatRow(payload []byte) string {
	n := int(binary.BigEndian.Uint16(payload[:2]))
	rest := payload[2:]
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ", "
		}
		length := int32(binary.BigEndian.Uint32(rest[:4]))
		rest = rest[4:]
		if length == -1 {
			out += "NULL"
			continue
		}
		out += string(rest[:length])
		rest = rest[length:]
	}
	return out
}

func formatError(payload []byte) string {
	out := ""
	for len(payload) > 0 && payload[0] != 0 {
		code := payload[0]
		val, rest, ok := wire.ReadCString(payload[1:])
		if !ok {
			break
		}
		payload = rest
		if code == 'C' {
			out += "SQLSTATE=" + val + " "
		}
		if code == 'M' {
			out += "message=" + val
		}
	}
	return out
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[client] RowDescription: 2 columns
[client] DataRow: 42, Alice
[client] DataRow: 99, NULL
[client] CommandComplete: "SELECT 2"
[client] ErrorResponse: SQLSTATE=42P01 message=relation "ghost" does not exist
```

### Tests

The tests assert the bytes that clients depend on: the SQLSTATE survives in the `ErrorResponse` payload, `ToWireError` preserves an existing `WireError` and tags an unknown error as internal, the command tags match Postgres exactly, the NULL column carries length `0xFFFFFFFF`, `DecodeBind` consumes the trailing result-format codes, and a Bind that omits them is rejected as `ErrShortMessage`.

Create `wire_test.go`:

```go
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

func newPipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	return c, s
}

func TestSendErrorResponseContainsSQLState(t *testing.T) {
	t.Parallel()
	c, s := newPipe(t)
	cf, sf := NewFramer(c), NewFramer(s)

	errc := make(chan error, 1)
	go func() {
		errc <- sf.SendErrorResponse(&WireError{
			Severity: "ERROR", Code: SQLStateSyntaxError, Message: "syntax error at end of input",
		})
	}()

	mt, payload, err := cf.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if mt != MsgErrorResponse {
		t.Errorf("type = %q, want MsgErrorResponse ('E')", mt)
	}
	if !strings.Contains(string(payload), SQLStateSyntaxError) {
		t.Errorf("payload %q missing SQLSTATE %q", payload, SQLStateSyntaxError)
	}
	if err := <-errc; err != nil {
		t.Errorf("SendErrorResponse: %v", err)
	}
}

func TestToWireError(t *testing.T) {
	t.Parallel()
	we := &WireError{Severity: "ERROR", Code: SQLStateSyntaxError, Message: "bad query"}
	if got := ToWireError(we); got != we {
		t.Error("ToWireError should return the identical *WireError")
	}
	plain := errors.New("unexpected condition")
	got := ToWireError(plain)
	if got.Code != SQLStateInternalError {
		t.Errorf("Code = %q, want %q", got.Code, SQLStateInternalError)
	}
	if got.Message != plain.Error() {
		t.Errorf("Message = %q, want %q", got.Message, plain.Error())
	}
}

func TestCommandTagTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cmd  string
		rows int
		want string
	}{
		{"SELECT", 0, "SELECT 0"},
		{"SELECT", 3, "SELECT 3"},
		{"INSERT", 1, "INSERT 0 1"},
		{"UPDATE", 5, "UPDATE 5"},
		{"DELETE", 2, "DELETE 2"},
		{"CREATE TABLE", 0, "CREATE TABLE"},
		{"select", 7, "SELECT 7"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := CommandTag(tc.cmd, tc.rows); got != tc.want {
				t.Errorf("CommandTag(%q, %d) = %q, want %q", tc.cmd, tc.rows, got, tc.want)
			}
		})
	}
}

func TestSendDataRowEncodesNull(t *testing.T) {
	t.Parallel()
	c, s := newPipe(t)
	cf, sf := NewFramer(c), NewFramer(s)

	errc := make(chan error, 1)
	go func() {
		if err := sf.SendDataRow([]*string{StringPtr("99"), nil}); err != nil {
			errc <- err
			return
		}
		errc <- sf.Flush()
	}()

	mt, payload, err := cf.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if mt != MsgDataRow {
		t.Errorf("type = %q, want MsgDataRow", mt)
	}
	// numCols(2) + firstLen(4) + "99"(2) = 8, then NULL length at [8:12].
	if len(payload) < 12 {
		t.Fatalf("payload too short: %d", len(payload))
	}
	if nullLen := binary.BigEndian.Uint32(payload[8:12]); nullLen != 0xFFFFFFFF {
		t.Errorf("NULL length = %#x, want 0xFFFFFFFF", nullLen)
	}
	if err := <-errc; err != nil {
		t.Errorf("server: %v", err)
	}
}

func TestDecodeParseRoundTrip(t *testing.T) {
	t.Parallel()
	var payload []byte
	payload = append(payload, "s1\x00"...)
	payload = append(payload, "SELECT $1\x00"...)
	payload = binary.BigEndian.AppendUint16(payload, 1)
	payload = binary.BigEndian.AppendUint32(payload, OIDInt4)

	stmt, err := DecodeParse(payload)
	if err != nil {
		t.Fatalf("DecodeParse: %v", err)
	}
	if stmt.Name != "s1" || stmt.Query != "SELECT $1" {
		t.Errorf("got name=%q query=%q", stmt.Name, stmt.Query)
	}
	if len(stmt.ParamTypes) != 1 || stmt.ParamTypes[0] != OIDInt4 {
		t.Errorf("ParamTypes = %v, want [%d]", stmt.ParamTypes, OIDInt4)
	}
}

func TestDecodeBindReturnsErrNoSuchStatement(t *testing.T) {
	t.Parallel()
	state := NewConnState()
	var payload []byte
	payload = append(payload, "\x00"...)        // unnamed portal
	payload = append(payload, "missing\x00"...) // nonexistent statement
	payload = binary.BigEndian.AppendUint16(payload, 0)
	payload = binary.BigEndian.AppendUint16(payload, 0)
	if _, err := DecodeBind(payload, state); !errors.Is(err, ErrNoSuchStatement) {
		t.Errorf("err = %v, want ErrNoSuchStatement", err)
	}
}

func TestDecodeBindConsumesResultFormats(t *testing.T) {
	t.Parallel()
	state := NewConnState()
	state.Statements["s1"] = &PreparedStatement{Name: "s1", Query: "SELECT $1", ParamTypes: []uint32{OIDText}}

	var payload []byte
	payload = append(payload, "\x00"...)                // unnamed portal
	payload = append(payload, "s1\x00"...)              // statement name
	payload = binary.BigEndian.AppendUint16(payload, 1) // 1 param format code
	payload = binary.BigEndian.AppendUint16(payload, 0) // text
	payload = binary.BigEndian.AppendUint16(payload, 1) // 1 param
	payload = binary.BigEndian.AppendUint32(payload, 2) // len("hi")
	payload = append(payload, "hi"...)
	payload = binary.BigEndian.AppendUint16(payload, 1) // 1 result format code
	payload = binary.BigEndian.AppendUint16(payload, 0) // text

	portal, err := DecodeBind(payload, state)
	if err != nil {
		t.Fatalf("DecodeBind: %v", err)
	}
	if len(portal.Params) != 1 || string(portal.Params[0]) != "hi" {
		t.Errorf("Params = %q, want [hi]", portal.Params)
	}
	if len(portal.ResultFormats) != 1 || portal.ResultFormats[0] != 0 {
		t.Errorf("ResultFormats = %v, want [0]", portal.ResultFormats)
	}
}

func TestDecodeBindMissingResultFormats(t *testing.T) {
	t.Parallel()
	state := NewConnState()
	state.Statements["s1"] = &PreparedStatement{Name: "s1", Query: "SELECT 1"}

	var payload []byte
	payload = append(payload, "\x00"...)                // unnamed portal
	payload = append(payload, "s1\x00"...)              // statement name
	payload = binary.BigEndian.AppendUint16(payload, 0) // 0 param formats
	payload = binary.BigEndian.AppendUint16(payload, 0) // 0 params
	// no result format count: must be rejected as truncated
	if _, err := DecodeBind(payload, state); !errors.Is(err, ErrShortMessage) {
		t.Errorf("err = %v, want ErrShortMessage", err)
	}
}

func TestConnStateIsolation(t *testing.T) {
	t.Parallel()
	s1, s2 := NewConnState(), NewConnState()
	s1.Statements["q"] = &PreparedStatement{Name: "q", Query: "SELECT 1"}
	if _, ok := s2.Statements["q"]; ok {
		t.Error("statement leaked across ConnState objects")
	}
}

func ExampleCommandTag() {
	fmt.Println(CommandTag("SELECT", 5))
	fmt.Println(CommandTag("INSERT", 1))
	fmt.Println(CommandTag("UPDATE", 3))
	// Output:
	// SELECT 5
	// INSERT 0 1
	// UPDATE 3
}
```

## Review

This module is correct when the result bytes are exactly what a client expects and the decoders account for every byte. A NULL column carries length `0xFFFFFFFF` and no data, distinct from a zero-length empty string. The command tags match Postgres to the character, including the legacy `0` in `INSERT 0 N`. The `ErrorResponse` carries the SQLSTATE in its `'C'` field so a driver classifies the failure correctly, and `ToWireError` guarantees there is always a SQLSTATE to send. `DecodeBind` consumes the parameter format codes, the parameter values, and the trailing result format codes, then rejects any leftover bytes; a Bind that stops short is an `ErrShortMessage`, not a panic.

The mistakes to avoid: encoding NULL as a zero-length value (clients then see a non-null empty string), and stopping `DecodeBind` after the parameter values (the unread result-format bytes desynchronize the very next message). Both are silent corruptions, which is why the tests assert on the exact NULL length and on the short-payload rejection.

## Resources

- [PostgreSQL: Message Formats](https://www.postgresql.org/docs/current/protocol-message-formats.html) — RowDescription, DataRow, CommandComplete, ErrorResponse, Bind byte layouts.
- [PostgreSQL: Error and Notice Message Fields](https://www.postgresql.org/docs/current/protocol-error-fields.html) — the field codes (`S`, `C`, `M`, `D`, `H`, `P`) an ErrorResponse carries.
- [PostgreSQL: Error Codes (SQLSTATE)](https://www.postgresql.org/docs/current/errcodes-appendix.html) — the five-character codes clients map to typed errors.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `binary.BigEndian` and the `AppendUint16`/`AppendUint32` helpers.

---

Back to [01-message-framing-and-startup.md](01-message-framing-and-startup.md) | Next: [03-concurrent-tcp-server.md](03-concurrent-tcp-server.md)
