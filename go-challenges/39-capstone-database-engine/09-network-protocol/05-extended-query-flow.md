# Exercise 5: Extended Query Flow (Parse / Bind / Describe / Execute / Sync)

The extended query protocol is how drivers like `pgx` run parameterized statements: it names a query with `Parse`, supplies typed parameters out of band with `Bind` (so values are never spliced into SQL text), inspects the result shape with `Describe`, runs a portal with `Execute`, and closes the cycle with `Sync`. This exercise builds the five messages as a standalone package with encoders, decoders that mirror them byte for byte, and a per-connection `ConnState` that tracks prepared statements and portals.

This module is fully self-contained. It depends only on the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
proto.go             Framer, Parse/Bind/Describe/Execute message types with Encode/Decode, ConnState
cmd/
  demo/
    main.go          drive a Parse/Bind/Describe/Execute/Sync flow over net.Pipe
proto_test.go        message round-trips over net.Pipe, the apply flow, and short-payload rejection
```

- Files: `proto.go`, `cmd/demo/main.go`, `proto_test.go`.
- Implement: `Framer`, `Parse`, `Bind`, `Describe`, `Execute` with `Encode`/`Decode`, `ConnState` with `ApplyParse`/`ApplyBind`, and the sentinels `ErrShort`/`ErrNoStatement`.
- Test: `proto_test.go` round-trips each message over `net.Pipe`, runs a Parse-then-Bind-then-Execute flow against a `ConnState`, and rejects truncated payloads as `ErrShort`.
- Verify: `go test -race ./...`

### Why parsing is split from execution

The simple protocol inlines parameters into the SQL string, which forces clients to escape values themselves (the source of SQL injection) and reparses the statement on every call. The extended protocol separates the phases so a statement is parsed once and run many times with different parameters, and the parameters travel as length-counted binary values that never touch the SQL text. `Parse` carries a statement name, the query, and the OIDs of its `$1`, `$2`, ... parameters. `Bind` references a parsed statement by name and supplies concrete values, producing a named portal — an executable instance. `Describe` asks for the result shape (a `RowDescription`, or `NoData` for a statement that returns nothing). `Execute` runs a portal, optionally capping the row count. `Sync` ends the cycle and forces a `ReadyForQuery`, which is the recovery boundary: even if a prior step failed, the client knows the server is ready again after `Sync`.

The encoders and decoders are exact inverses, and `DecodeBind` is the one that earns its complexity. A Bind payload is: portal name, statement name, an `int16` count of parameter format codes plus those codes, an `int16` parameter count plus each parameter's `int32` length and bytes (or `-1` for NULL), and then the part decoders forget — an `int16` count of result format codes plus those codes. A decoder that stops after the parameter values leaves the result-format bytes in the stream, so the next `ReadMessage` misreads them as a frame header and the connection desynchronizes. The decoder here consumes them with a shared `skipFormatCodes` helper and treats any leftover bytes as a protocol error, so every Bind is accounted for to the last byte. `ConnState` keeps the statements and portals for one connection only; `ApplyBind` against an unknown statement returns `ErrNoStatement` rather than creating a dangling portal.

Create `proto.go`:

```go
// Package extended implements encode/decode for the PostgreSQL extended query
// protocol messages: Parse, Bind, Describe, Execute, Sync. Each message frames
// as type + int32(length, includes itself) + payload and round-trips through a
// Framer built on bufio + io.ReadFull.
package extended

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// Frontend extended-query message type bytes.
const (
	MsgParse    byte = 'P'
	MsgBind     byte = 'B'
	MsgDescribe byte = 'D'
	MsgExecute  byte = 'E'
	MsgSync     byte = 'S'
)

// Sentinel errors. Wrap with %w so callers can errors.Is them.
var (
	ErrShort       = errors.New("extended: payload truncated")
	ErrNoStatement = errors.New("extended: no such prepared statement")
)

// Framer reads and writes length-prefixed messages over a net.Conn.
type Framer struct {
	r *bufio.Reader
	w *bufio.Writer
}

// NewFramer wraps c with buffered I/O.
func NewFramer(c net.Conn) *Framer {
	return &Framer{r: bufio.NewReader(c), w: bufio.NewWriter(c)}
}

// WriteMessage writes type + int32(len+4) + payload and flushes.
func (f *Framer) WriteMessage(t byte, payload []byte) error {
	if err := f.w.WriteByte(t); err != nil {
		return fmt.Errorf("extended: write type: %w", err)
	}
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(payload)+4))
	if _, err := f.w.Write(lb[:]); err != nil {
		return fmt.Errorf("extended: write len: %w", err)
	}
	if _, err := f.w.Write(payload); err != nil {
		return fmt.Errorf("extended: write payload: %w", err)
	}
	return f.w.Flush()
}

// ReadMessage reads one message, using io.ReadFull for every fixed-size field so
// a partial Read never truncates the frame.
func (f *Framer) ReadMessage() (byte, []byte, error) {
	t, err := f.r.ReadByte()
	if err != nil {
		return 0, nil, fmt.Errorf("extended: read type: %w", err)
	}
	var lb [4]byte
	if _, err := io.ReadFull(f.r, lb[:]); err != nil {
		return 0, nil, fmt.Errorf("extended: read len: %w", err)
	}
	n := binary.BigEndian.Uint32(lb[:])
	if n < 4 {
		return 0, nil, fmt.Errorf("extended: len %d < 4: %w", n, ErrShort)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(f.r, payload); err != nil {
		return 0, nil, fmt.Errorf("extended: read payload: %w", err)
	}
	return t, payload, nil
}

// Parse names a query and declares its parameter type OIDs.
type Parse struct {
	Name      string
	Query     string
	ParamOIDs []uint32
}

// Encode serializes p into a Parse payload.
func (p Parse) Encode() []byte {
	b := appendCString(nil, p.Name)
	b = appendCString(b, p.Query)
	b = binary.BigEndian.AppendUint16(b, uint16(len(p.ParamOIDs)))
	for _, o := range p.ParamOIDs {
		b = binary.BigEndian.AppendUint32(b, o)
	}
	return b
}

// DecodeParse parses a Parse payload.
func DecodeParse(payload []byte) (Parse, error) {
	name, rest, ok := readCString(payload)
	if !ok {
		return Parse{}, fmt.Errorf("extended: parse name: %w", ErrShort)
	}
	query, rest, ok := readCString(rest)
	if !ok {
		return Parse{}, fmt.Errorf("extended: parse query: %w", ErrShort)
	}
	if len(rest) < 2 {
		return Parse{}, fmt.Errorf("extended: parse count: %w", ErrShort)
	}
	n := int(binary.BigEndian.Uint16(rest[:2]))
	rest = rest[2:]
	if len(rest) < n*4 {
		return Parse{}, fmt.Errorf("extended: parse oids: %w", ErrShort)
	}
	oids := make([]uint32, n)
	for i := range oids {
		oids[i] = binary.BigEndian.Uint32(rest[i*4:])
	}
	return Parse{Name: name, Query: query, ParamOIDs: oids}, nil
}

// Bind supplies parameter values for a prepared statement. A nil Params element
// is SQL NULL. This decoder accepts text-format parameters and consumes the
// trailing result-format codes so the stream stays aligned.
type Bind struct {
	Portal    string
	Statement string
	Params    [][]byte
}

// Encode serializes b into a Bind payload (0 param formats, 0 result formats).
func (b Bind) Encode() []byte {
	out := appendCString(nil, b.Portal)
	out = appendCString(out, b.Statement)
	out = binary.BigEndian.AppendUint16(out, 0) // 0 param format codes = all text
	out = binary.BigEndian.AppendUint16(out, uint16(len(b.Params)))
	for _, p := range b.Params {
		if p == nil {
			out = binary.BigEndian.AppendUint32(out, 0xFFFFFFFF) // NULL
			continue
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(p)))
		out = append(out, p...)
	}
	out = binary.BigEndian.AppendUint16(out, 0) // 0 result format codes
	return out
}

// DecodeBind parses a Bind payload, consuming param format codes, parameter
// values, and result format codes. Leftover bytes are a protocol error.
func DecodeBind(payload []byte) (Bind, error) {
	portal, rest, ok := readCString(payload)
	if !ok {
		return Bind{}, fmt.Errorf("extended: bind portal: %w", ErrShort)
	}
	stmt, rest, ok := readCString(rest)
	if !ok {
		return Bind{}, fmt.Errorf("extended: bind statement: %w", ErrShort)
	}
	rest, err := skipFormatCodes(rest, "param")
	if err != nil {
		return Bind{}, err
	}
	if len(rest) < 2 {
		return Bind{}, fmt.Errorf("extended: bind param count: %w", ErrShort)
	}
	np := int(binary.BigEndian.Uint16(rest[:2]))
	rest = rest[2:]
	params := make([][]byte, np)
	for i := range params {
		if len(rest) < 4 {
			return Bind{}, fmt.Errorf("extended: bind param %d len: %w", i, ErrShort)
		}
		l := int32(binary.BigEndian.Uint32(rest[:4]))
		rest = rest[4:]
		if l == -1 {
			params[i] = nil
			continue
		}
		if int(l) > len(rest) {
			return Bind{}, fmt.Errorf("extended: bind param %d data: %w", i, ErrShort)
		}
		params[i] = rest[:l]
		rest = rest[l:]
	}
	rest, err = skipFormatCodes(rest, "result")
	if err != nil {
		return Bind{}, err
	}
	if len(rest) != 0 {
		return Bind{}, fmt.Errorf("extended: bind: %d trailing bytes", len(rest))
	}
	return Bind{Portal: portal, Statement: stmt, Params: params}, nil
}

// skipFormatCodes consumes an int16 count followed by that many int16 codes.
func skipFormatCodes(rest []byte, kind string) ([]byte, error) {
	if len(rest) < 2 {
		return nil, fmt.Errorf("extended: bind %s format count: %w", kind, ErrShort)
	}
	n := int(binary.BigEndian.Uint16(rest[:2]))
	if len(rest) < 2+n*2 {
		return nil, fmt.Errorf("extended: bind %s format codes: %w", kind, ErrShort)
	}
	return rest[2+n*2:], nil
}

// Describe targets either a prepared statement ('S') or a portal ('P').
type Describe struct {
	Kind byte
	Name string
}

// Encode serializes d into a Describe payload.
func (d Describe) Encode() []byte {
	return appendCString([]byte{d.Kind}, d.Name)
}

// DecodeDescribe parses a Describe payload.
func DecodeDescribe(payload []byte) (Describe, error) {
	if len(payload) < 1 {
		return Describe{}, fmt.Errorf("extended: describe kind: %w", ErrShort)
	}
	name, _, ok := readCString(payload[1:])
	if !ok {
		return Describe{}, fmt.Errorf("extended: describe name: %w", ErrShort)
	}
	return Describe{Kind: payload[0], Name: name}, nil
}

// Execute runs a portal, returning at most MaxRows rows (0 = unlimited).
type Execute struct {
	Portal  string
	MaxRows uint32
}

// Encode serializes e into an Execute payload.
func (e Execute) Encode() []byte {
	b := appendCString(nil, e.Portal)
	return binary.BigEndian.AppendUint32(b, e.MaxRows)
}

// DecodeExecute parses an Execute payload.
func DecodeExecute(payload []byte) (Execute, error) {
	name, rest, ok := readCString(payload)
	if !ok {
		return Execute{}, fmt.Errorf("extended: execute portal: %w", ErrShort)
	}
	if len(rest) < 4 {
		return Execute{}, fmt.Errorf("extended: execute max rows: %w", ErrShort)
	}
	return Execute{Portal: name, MaxRows: binary.BigEndian.Uint32(rest[:4])}, nil
}

// PreparedStatement is the server-side result of applying a Parse.
type PreparedStatement struct {
	Name      string
	Query     string
	ParamOIDs []uint32
}

// Portal is the server-side result of applying a Bind to a statement.
type Portal struct {
	Name      string
	Statement *PreparedStatement
	Params    [][]byte
}

// ConnState holds one connection's prepared statements and portals. It is never
// shared between connections.
type ConnState struct {
	Statements map[string]*PreparedStatement
	Portals    map[string]*Portal
}

// NewConnState allocates an empty ConnState.
func NewConnState() *ConnState {
	return &ConnState{
		Statements: make(map[string]*PreparedStatement),
		Portals:    make(map[string]*Portal),
	}
}

// ApplyParse registers a prepared statement.
func (s *ConnState) ApplyParse(p Parse) {
	s.Statements[p.Name] = &PreparedStatement{Name: p.Name, Query: p.Query, ParamOIDs: p.ParamOIDs}
}

// ApplyBind creates a portal from a bound statement, or returns ErrNoStatement.
func (s *ConnState) ApplyBind(b Bind) (*Portal, error) {
	st, ok := s.Statements[b.Statement]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoStatement, b.Statement)
	}
	po := &Portal{Name: b.Portal, Statement: st, Params: b.Params}
	s.Portals[b.Portal] = po
	return po, nil
}

// appendCString appends s and a terminating null byte.
func appendCString(b []byte, s string) []byte {
	b = append(b, s...)
	return append(b, 0)
}

// readCString reads a null-terminated string and returns the remainder.
func readCString(data []byte) (s string, rest []byte, ok bool) {
	for i, c := range data {
		if c == 0 {
			return string(data[:i]), data[i+1:], true
		}
	}
	return "", data, false
}
```

### The runnable demo

The demo runs a client and a server over `net.Pipe`. The client encodes the five messages and sends them; the server decodes each one, applies `Parse` and `Bind` to its `ConnState`, and prints what it observed. Because the pipe is synchronous, each message is fully received before the next is sent, so the output order is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net"
	"os"

	"example.com/extended"
)

func main() {
	c, s := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := runServer(s); err != nil {
			fmt.Fprintln(os.Stderr, "server:", err)
		}
	}()

	cf := extended.NewFramer(c)
	send := func(t byte, payload []byte) {
		if err := cf.WriteMessage(t, payload); err != nil {
			fmt.Fprintln(os.Stderr, "client:", err)
		}
	}
	send(extended.MsgParse, extended.Parse{Name: "s1", Query: "SELECT $1", ParamOIDs: []uint32{23}}.Encode())
	send(extended.MsgBind, extended.Bind{Portal: "p1", Statement: "s1", Params: [][]byte{[]byte("7")}}.Encode())
	send(extended.MsgDescribe, extended.Describe{Kind: 'P', Name: "p1"}.Encode())
	send(extended.MsgExecute, extended.Execute{Portal: "p1", MaxRows: 0}.Encode())
	send(extended.MsgSync, nil)
	<-done
	c.Close()
}

func runServer(conn net.Conn) error {
	defer conn.Close()
	f := extended.NewFramer(conn)
	state := extended.NewConnState()
	for {
		t, payload, err := f.ReadMessage()
		if err != nil {
			return err
		}
		switch t {
		case extended.MsgParse:
			p, err := extended.DecodeParse(payload)
			if err != nil {
				return err
			}
			state.ApplyParse(p)
			fmt.Printf("[server] Parse: name=%q query=%q oids=%v\n", p.Name, p.Query, p.ParamOIDs)
		case extended.MsgBind:
			b, err := extended.DecodeBind(payload)
			if err != nil {
				return err
			}
			portal, err := state.ApplyBind(b)
			if err != nil {
				return err
			}
			fmt.Printf("[server] Bind: portal=%q stmt=%q params=%s\n", portal.Name, portal.Statement.Name, paramsString(portal.Params))
		case extended.MsgDescribe:
			d, err := extended.DecodeDescribe(payload)
			if err != nil {
				return err
			}
			fmt.Printf("[server] Describe: kind=%c name=%q\n", d.Kind, d.Name)
		case extended.MsgExecute:
			e, err := extended.DecodeExecute(payload)
			if err != nil {
				return err
			}
			fmt.Printf("[server] Execute: portal=%q maxRows=%d\n", e.Portal, e.MaxRows)
		case extended.MsgSync:
			fmt.Println("[server] Sync -> ReadyForQuery")
			return nil
		}
	}
}

func paramsString(params [][]byte) string {
	out := "["
	for i, p := range params {
		if i > 0 {
			out += " "
		}
		if p == nil {
			out += "NULL"
		} else {
			out += string(p)
		}
	}
	return out + "]"
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[server] Parse: name="s1" query="SELECT $1" oids=[23]
[server] Bind: portal="p1" stmt="s1" params=[7]
[server] Describe: kind=P name="p1"
[server] Execute: portal="p1" maxRows=0
[server] Sync -> ReadyForQuery
```

### Tests

The tests prove the encoders and decoders are inverses and the apply flow tracks state. A table round-trips each message type over `net.Pipe`. A flow test runs Parse, then a decoded Bind through `ApplyBind`, then a decoded Execute, and checks the portal carries the right statement and parameter. An `ApplyBind` against an unknown statement must be `ErrNoStatement`, and every decoder must reject a truncated payload as `ErrShort`.

Create `proto_test.go`:

```go
package extended

import (
	"errors"
	"net"
	"testing"
)

func TestMessageRoundTripOverPipe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		typ  byte
		enc  []byte
	}{
		{"parse", MsgParse, Parse{Name: "s1", Query: "SELECT $1", ParamOIDs: []uint32{23}}.Encode()},
		{"bind", MsgBind, Bind{Portal: "", Statement: "s1", Params: [][]byte{[]byte("42"), nil}}.Encode()},
		{"describe", MsgDescribe, Describe{Kind: 'P', Name: ""}.Encode()},
		{"execute", MsgExecute, Execute{Portal: "", MaxRows: 100}.Encode()},
		{"sync", MsgSync, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, s := net.Pipe()
			t.Cleanup(func() { c.Close(); s.Close() })
			cf, sf := NewFramer(c), NewFramer(s)

			errc := make(chan error, 1)
			go func() { errc <- cf.WriteMessage(tc.typ, tc.enc) }()

			typ, payload, err := sf.ReadMessage()
			if err != nil {
				t.Fatalf("ReadMessage: %v", err)
			}
			if typ != tc.typ {
				t.Errorf("type = %q, want %q", typ, tc.typ)
			}
			if string(payload) != string(tc.enc) {
				t.Errorf("payload = %x, want %x", payload, tc.enc)
			}
			if err := <-errc; err != nil {
				t.Errorf("WriteMessage: %v", err)
			}
		})
	}
}

func TestParseBindExecuteFlow(t *testing.T) {
	t.Parallel()
	state := NewConnState()
	state.ApplyParse(Parse{Name: "s1", Query: "SELECT $1", ParamOIDs: []uint32{23}})

	bind, err := DecodeBind(Bind{Portal: "p1", Statement: "s1", Params: [][]byte{[]byte("7")}}.Encode())
	if err != nil {
		t.Fatalf("DecodeBind: %v", err)
	}
	portal, err := state.ApplyBind(bind)
	if err != nil {
		t.Fatalf("ApplyBind: %v", err)
	}
	if portal.Statement.Query != "SELECT $1" {
		t.Errorf("portal query = %q, want %q", portal.Statement.Query, "SELECT $1")
	}
	if len(portal.Params) != 1 || string(portal.Params[0]) != "7" {
		t.Errorf("portal params = %q, want [7]", portal.Params)
	}

	exec, err := DecodeExecute(Execute{Portal: "p1", MaxRows: 0}.Encode())
	if err != nil {
		t.Fatalf("DecodeExecute: %v", err)
	}
	if _, ok := state.Portals[exec.Portal]; !ok {
		t.Errorf("portal %q not found for Execute", exec.Portal)
	}
}

func TestApplyBindMissingStatement(t *testing.T) {
	t.Parallel()
	state := NewConnState()
	if _, err := state.ApplyBind(Bind{Portal: "p", Statement: "ghost"}); !errors.Is(err, ErrNoStatement) {
		t.Errorf("err = %v, want ErrNoStatement", err)
	}
}

func TestUnnamedPortalOverwrites(t *testing.T) {
	t.Parallel()
	state := NewConnState()
	state.ApplyParse(Parse{Name: "s1", Query: "SELECT 1"})
	state.ApplyParse(Parse{Name: "s2", Query: "SELECT 2"})

	if _, err := state.ApplyBind(Bind{Portal: "", Statement: "s1"}); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if _, err := state.ApplyBind(Bind{Portal: "", Statement: "s2"}); err != nil {
		t.Fatalf("second bind: %v", err)
	}
	if len(state.Portals) != 1 {
		t.Errorf("portal count = %d, want 1 (unnamed portal should overwrite)", len(state.Portals))
	}
	if state.Portals[""].Statement.Name != "s2" {
		t.Errorf("unnamed portal stmt = %q, want s2", state.Portals[""].Statement.Name)
	}
}

func TestDecodeShortPayloads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fn   func() error
	}{
		{"parse", func() error { _, err := DecodeParse([]byte("s1")); return err }},
		{"bind", func() error { _, err := DecodeBind([]byte("p")); return err }},
		{"describe", func() error { _, err := DecodeDescribe(nil); return err }},
		{"execute", func() error { _, err := DecodeExecute([]byte("p\x00\x00")); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.fn(); !errors.Is(err, ErrShort) {
				t.Errorf("err = %v, want ErrShort", err)
			}
		})
	}
}
```

## Review

The extended flow is correct when encode and decode are exact inverses and the apply step tracks per-connection state. Each message round-trips over `net.Pipe` byte for byte; `DecodeBind` consumes the parameter format codes, the parameter values, and the trailing result format codes, rejecting any leftover bytes. `ApplyParse` registers a statement and `ApplyBind` produces a portal pointing at it, returning `ErrNoStatement` for an unknown name, and an unnamed (`""`) portal overwrites the previous unnamed portal rather than accumulating — exactly as a driver pipelining anonymous statements expects.

The mistake to avoid is the same one that breaks the simple-protocol Bind: stopping `DecodeBind` after the parameter values. The result-format codes are not optional padding; leaving them in the buffer corrupts the next message. Decoding every fixed field through length checks that return `ErrShort` keeps a truncated payload a classifiable error rather than a panic.

## Resources

- [PostgreSQL: Protocol Flow — Extended Query](https://www.postgresql.org/docs/current/protocol-flow.html#PROTOCOL-FLOW-EXTENDED-QUERY) — the Parse/Bind/Describe/Execute/Sync sequence and the role of Sync.
- [PostgreSQL: Message Formats](https://www.postgresql.org/docs/current/protocol-message-formats.html) — the byte layout of Parse, Bind, Describe, and Execute, including the format-code arrays.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `binary.BigEndian` and `AppendUint16`/`AppendUint32`.
- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull) — the exact-count read every fixed-size field uses.

---

Back to [04-client-server-round-trip.md](04-client-server-round-trip.md) | Next: [06-streaming-result-set.md](06-streaming-result-set.md)
