# Exercise 7: Startup and Authentication Handshake

Before any query flows, the connection completes a startup and authentication handshake. This exercise encodes and decodes the messages of that phase: the typeless `StartupMessage`, the `Authentication` messages (`AuthenticationOk` and the cleartext-password request), the client's `PasswordMessage`, one `ParameterStatus` per server setting, `BackendKeyData` for cancellation, and the `ReadyForQuery` that hands control to the client. A server helper emits the whole post-authentication sequence in order, and `ReadStartup` rejects any protocol version other than 3.0 with a sentinel.

This module is fully self-contained. It depends only on the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
handshake.go         Framer, StartupMessage, Authentication (+ cleartext password), ParameterStatus,
                     BackendKeyData, ReadyForQuery, PasswordMessage, SendServerHandshake
cmd/
  demo/
    main.go          a full cleartext-password handshake over net.Pipe
handshake_test.go    a full handshake round-trip, bad-version rejection, password exchange, short payloads
```

- Files: `handshake.go`, `cmd/demo/main.go`, `handshake_test.go`.
- Implement: `(*Framer).WriteStartup`/`ReadStartup`, `WriteAuth`/`DecodeAuth`, `WritePassword`/`DecodePassword`, `WriteParamStatus`/`DecodeParamStatus`, `WriteBackendKeyData`/`DecodeBackendKeyData`, `WriteReadyForQuery`/`DecodeReadyForQuery`, `SendServerHandshake`, and the sentinels `ErrShort`/`ErrBadVersion`.
- Test: `handshake_test.go` runs a full handshake over `net.Pipe`, rejects protocol version 3.1, completes a cleartext-password exchange, and rejects truncated payloads as `ErrShort`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p handshake/cmd/demo && cd handshake
go mod init example.com/handshake
```

### The shape of the handshake

The handshake is the one part of the protocol where the message order is fixed and the framing rules bend. The client opens with a `StartupMessage`: the four-byte length is the very first thing on the wire (no type byte), followed by an `int32` protocol version and null-terminated `key\0value\0` pairs ending in an empty key. `ReadStartup` reads the length directly and rejects any version other than 3.0 (`196608`) with `ErrBadVersion` — a server must never try to parse an unknown layout. Encoding the parameter keys in sorted order makes the wire bytes deterministic, which is what lets a test assert on them.

Authentication is a small negotiation carried entirely by the `Authentication` message (type `'R'`), whose meaning is the `int32` code in its payload. `AuthenticationOk` is code `0`; `AuthenticationCleartextPassword` is code `3`, a request for the client to reply with a `PasswordMessage` (type `'p'`, a null-terminated password). Real servers use stronger methods (SCRAM), but cleartext is the minimal exchange that exercises the request/response shape: server sends the request, client answers with the password, server validates and proceeds. Because both `AuthenticationOk` and the cleartext request share the type byte `'R'`, the client must decode the code to tell them apart — a recurring lesson that the type byte alone is not enough.

After authentication succeeds the server sends the rest of the sequence: one `ParameterStatus` (`name\0value\0`) per server setting so the client learns the server's encoding and version, then `BackendKeyData` (an `int32` process ID and an `int32` secret used to match a later cancel request), then `ReadyForQuery` carrying the transaction status byte (`'I'` idle). `SendServerHandshake` performs `AuthenticationOk` through `ReadyForQuery` in order and flushes, since the client blocks on that final `ReadyForQuery`. Every decoder bounds-checks its payload and returns `ErrShort` on a truncated buffer, and every fixed-size read uses `io.ReadFull`.

Create `handshake.go`:

```go
// Package handshake encodes and decodes the PostgreSQL startup and
// authentication handshake: StartupMessage (no type byte), AuthenticationOk /
// AuthenticationCleartextPassword, PasswordMessage, ParameterStatus,
// BackendKeyData, and ReadyForQuery.
package handshake

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
)

// ProtocolVersion3 is the 3.0 startup version: (major 3 << 16) | minor 0.
const ProtocolVersion3 int32 = 196608

// Message type bytes used by the handshake.
const (
	MsgAuth          byte = 'R'
	MsgPassword      byte = 'p' // frontend PasswordMessage
	MsgParamStatus   byte = 'S'
	MsgBackendKey    byte = 'K'
	MsgReadyForQuery byte = 'Z'
)

// Authentication request codes (the int32 in an 'R' message).
const (
	AuthOK        int32 = 0
	AuthCleartext int32 = 3
)

// Transaction-status bytes for ReadyForQuery.
const (
	TxIdle byte = 'I'
	TxInTx byte = 'T'
	TxFail byte = 'E'
)

// Sentinel errors. Wrap with %w so callers can errors.Is them.
var (
	ErrShort      = errors.New("handshake: payload truncated")
	ErrBadVersion = errors.New("handshake: unsupported protocol version")
)

// Framer reads and writes handshake messages over a net.Conn.
type Framer struct {
	r *bufio.Reader
	w *bufio.Writer
}

// NewFramer wraps c with buffered I/O.
func NewFramer(c net.Conn) *Framer {
	return &Framer{r: bufio.NewReader(c), w: bufio.NewWriter(c)}
}

// Flush flushes buffered writes.
func (f *Framer) Flush() error { return f.w.Flush() }

// WriteStartup writes a StartupMessage: int32(len) + int32(version) + sorted
// key\0value\0 pairs + a terminating null. There is no type byte.
func (f *Framer) WriteStartup(params map[string]string) error {
	var body []byte
	body = binary.BigEndian.AppendUint32(body, uint32(ProtocolVersion3))
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		body = appendCString(body, k)
		body = appendCString(body, params[k])
	}
	body = append(body, 0)

	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)+4))
	if _, err := f.w.Write(hdr[:]); err != nil {
		return fmt.Errorf("handshake: startup header: %w", err)
	}
	if _, err := f.w.Write(body); err != nil {
		return fmt.Errorf("handshake: startup body: %w", err)
	}
	return f.w.Flush()
}

// ReadStartup reads a StartupMessage. The four-byte length is first; there is no
// type byte. A version other than 3.0 returns ErrBadVersion.
func (f *Framer) ReadStartup() (int32, map[string]string, error) {
	var lb [4]byte
	if _, err := io.ReadFull(f.r, lb[:]); err != nil {
		return 0, nil, fmt.Errorf("handshake: startup len: %w", err)
	}
	n := binary.BigEndian.Uint32(lb[:])
	if n < 8 {
		return 0, nil, fmt.Errorf("handshake: startup len %d < 8: %w", n, ErrShort)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(f.r, body); err != nil {
		return 0, nil, fmt.Errorf("handshake: startup body: %w", err)
	}
	ver := int32(binary.BigEndian.Uint32(body[:4]))
	if ver != ProtocolVersion3 {
		return ver, nil, fmt.Errorf("%w: %d", ErrBadVersion, ver)
	}
	params := make(map[string]string)
	rest := body[4:]
	for len(rest) > 0 {
		k, r2, ok := readCString(rest)
		if !ok || k == "" {
			break
		}
		v, r3, ok := readCString(r2)
		if !ok {
			return ver, nil, fmt.Errorf("handshake: startup value: %w", ErrShort)
		}
		params[k] = v
		rest = r3
	}
	return ver, params, nil
}

// WriteMessage writes a typed message: type + int32(len+4) + payload.
func (f *Framer) WriteMessage(t byte, payload []byte) error {
	if err := f.w.WriteByte(t); err != nil {
		return fmt.Errorf("handshake: write type: %w", err)
	}
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(payload)+4))
	if _, err := f.w.Write(lb[:]); err != nil {
		return fmt.Errorf("handshake: write len: %w", err)
	}
	if _, err := f.w.Write(payload); err != nil {
		return fmt.Errorf("handshake: write payload: %w", err)
	}
	return nil
}

// ReadMessage reads one typed message using io.ReadFull throughout.
func (f *Framer) ReadMessage() (byte, []byte, error) {
	t, err := f.r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var lb [4]byte
	if _, err := io.ReadFull(f.r, lb[:]); err != nil {
		return 0, nil, fmt.Errorf("handshake: read len: %w", err)
	}
	n := binary.BigEndian.Uint32(lb[:])
	if n < 4 {
		return 0, nil, fmt.Errorf("handshake: len %d < 4: %w", n, ErrShort)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(f.r, payload); err != nil {
		return 0, nil, fmt.Errorf("handshake: read payload: %w", err)
	}
	return t, payload, nil
}

// WriteAuth writes an Authentication message carrying the given code.
func (f *Framer) WriteAuth(code int32) error {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], uint32(code))
	return f.WriteMessage(MsgAuth, p[:])
}

// DecodeAuth reads the int32 code from an Authentication payload.
func DecodeAuth(payload []byte) (int32, error) {
	if len(payload) < 4 {
		return 0, fmt.Errorf("handshake: auth code: %w", ErrShort)
	}
	return int32(binary.BigEndian.Uint32(payload[:4])), nil
}

// WritePassword writes a PasswordMessage (type 'p', null-terminated password).
func (f *Framer) WritePassword(password string) error {
	return f.WriteMessage(MsgPassword, appendCString(nil, password))
}

// DecodePassword parses a PasswordMessage payload.
func DecodePassword(payload []byte) (string, error) {
	pw, _, ok := readCString(payload)
	if !ok {
		return "", fmt.Errorf("handshake: password: %w", ErrShort)
	}
	return pw, nil
}

// WriteParamStatus writes a ParameterStatus (name\0value\0).
func (f *Framer) WriteParamStatus(name, value string) error {
	p := appendCString(nil, name)
	p = appendCString(p, value)
	return f.WriteMessage(MsgParamStatus, p)
}

// DecodeParamStatus parses a ParameterStatus payload.
func DecodeParamStatus(payload []byte) (name, value string, err error) {
	name, rest, ok := readCString(payload)
	if !ok {
		return "", "", fmt.Errorf("handshake: param name: %w", ErrShort)
	}
	value, _, ok = readCString(rest)
	if !ok {
		return "", "", fmt.Errorf("handshake: param value: %w", ErrShort)
	}
	return name, value, nil
}

// WriteBackendKeyData writes BackendKeyData (int32 pid + int32 secret).
func (f *Framer) WriteBackendKeyData(pid, secret int32) error {
	var p [8]byte
	binary.BigEndian.PutUint32(p[:4], uint32(pid))
	binary.BigEndian.PutUint32(p[4:], uint32(secret))
	return f.WriteMessage(MsgBackendKey, p[:])
}

// DecodeBackendKeyData parses a BackendKeyData payload.
func DecodeBackendKeyData(payload []byte) (pid, secret int32, err error) {
	if len(payload) < 8 {
		return 0, 0, fmt.Errorf("handshake: backend key data: %w", ErrShort)
	}
	return int32(binary.BigEndian.Uint32(payload[:4])),
		int32(binary.BigEndian.Uint32(payload[4:8])), nil
}

// WriteReadyForQuery writes ReadyForQuery with the given transaction status.
func (f *Framer) WriteReadyForQuery(tx byte) error {
	return f.WriteMessage(MsgReadyForQuery, []byte{tx})
}

// DecodeReadyForQuery parses the single transaction-status byte.
func DecodeReadyForQuery(payload []byte) (byte, error) {
	if len(payload) < 1 {
		return 0, fmt.Errorf("handshake: ready status: %w", ErrShort)
	}
	return payload[0], nil
}

// SendServerHandshake performs the post-authentication sequence:
// AuthenticationOk -> ParameterStatus* -> BackendKeyData -> ReadyForQuery(idle),
// then flushes.
func (f *Framer) SendServerHandshake(params map[string]string, pid, secret int32) error {
	if err := f.WriteAuth(AuthOK); err != nil {
		return err
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := f.WriteParamStatus(k, params[k]); err != nil {
			return err
		}
	}
	if err := f.WriteBackendKeyData(pid, secret); err != nil {
		return err
	}
	if err := f.WriteReadyForQuery(TxIdle); err != nil {
		return err
	}
	return f.Flush()
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

The demo runs a full cleartext-password handshake over `net.Pipe`. The client sends a startup message; the server requests a cleartext password; the client replies with one; the server validates it and sends the post-authentication sequence; the client reads through to `ReadyForQuery`. Because both `Authentication` messages share the type byte `'R'`, the client decodes the code to distinguish the password request from the OK.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net"
	"os"

	"example.com/handshake"
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

	f := handshake.NewFramer(c)
	if err := f.WriteStartup(map[string]string{"user": "demo", "database": "testdb"}); err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		os.Exit(1)
	}
	for {
		t, payload, err := f.ReadMessage()
		if err != nil {
			fmt.Fprintln(os.Stderr, "client:", err)
			os.Exit(1)
		}
		switch t {
		case handshake.MsgAuth:
			code, _ := handshake.DecodeAuth(payload)
			switch code {
			case handshake.AuthCleartext:
				fmt.Println("[client] password requested")
				if err := f.WritePassword("secret"); err != nil {
					fmt.Fprintln(os.Stderr, "client:", err)
					os.Exit(1)
				}
				_ = f.Flush()
			case handshake.AuthOK:
				fmt.Println("[client] AuthenticationOk")
			}
		case handshake.MsgParamStatus:
			name, value, _ := handshake.DecodeParamStatus(payload)
			fmt.Printf("[client] ParameterStatus: %s=%s\n", name, value)
		case handshake.MsgBackendKey:
			pid, secret, _ := handshake.DecodeBackendKeyData(payload)
			fmt.Printf("[client] BackendKeyData pid=%d secret=%d\n", pid, secret)
		case handshake.MsgReadyForQuery:
			tx, _ := handshake.DecodeReadyForQuery(payload)
			fmt.Printf("[client] ReadyForQuery status=%c\n", tx)
			c.Close()
			<-done
			return
		}
	}
}

func runServer(conn net.Conn) error {
	f := handshake.NewFramer(conn)
	_, params, err := f.ReadStartup()
	if err != nil {
		return err
	}
	fmt.Printf("[server] startup: user=%q database=%q\n", params["user"], params["database"])

	if err := f.WriteAuth(handshake.AuthCleartext); err != nil {
		return err
	}
	if err := f.Flush(); err != nil {
		return err
	}
	t, payload, err := f.ReadMessage()
	if err != nil {
		return err
	}
	if t != handshake.MsgPassword {
		return fmt.Errorf("expected PasswordMessage, got %q", t)
	}
	pw, err := handshake.DecodePassword(payload)
	if err != nil {
		return err
	}
	fmt.Printf("[server] password received: %q\n", pw)

	return f.SendServerHandshake(
		map[string]string{"server_version": "15.0", "client_encoding": "UTF8"},
		4242, 99,
	)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[server] startup: user="demo" database="testdb"
[client] password requested
[server] password received: "secret"
[client] AuthenticationOk
[client] ParameterStatus: client_encoding=UTF8
[client] ParameterStatus: server_version=15.0
[client] BackendKeyData pid=4242 secret=99
[client] ReadyForQuery status=I
```

### Tests

The tests pin the handshake shape. A full round-trip checks the startup parameters survive and that the server sequence arrives as AuthOK, two ParameterStatus, BackendKeyData, ReadyForQuery in order. A version test confirms 3.1 is rejected with `ErrBadVersion`. A password test runs the cleartext exchange and checks the server decodes the client's password. A short-payload table pins the `ErrShort` boundaries.

Create `handshake_test.go`:

```go
package handshake

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"
)

func TestFullHandshakeOverPipe(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	client, server := NewFramer(c), NewFramer(s)

	clientErr := make(chan error, 1)
	go func() {
		clientErr <- client.WriteStartup(map[string]string{"user": "alice", "database": "mydb"})
	}()

	ver, params, err := server.ReadStartup()
	if err != nil {
		t.Fatalf("ReadStartup: %v", err)
	}
	if ver != ProtocolVersion3 {
		t.Errorf("version = %d, want %d", ver, ProtocolVersion3)
	}
	if params["user"] != "alice" || params["database"] != "mydb" {
		t.Errorf("params = %v", params)
	}
	if err := <-clientErr; err != nil {
		t.Fatalf("WriteStartup: %v", err)
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.SendServerHandshake(
			map[string]string{"server_version": "15.0", "client_encoding": "UTF8"}, 4242, 99)
	}()

	typ, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}
	if typ != MsgAuth {
		t.Fatalf("type = %q, want auth", typ)
	}
	if code, _ := DecodeAuth(payload); code != AuthOK {
		t.Errorf("auth code = %d, want AuthOK", code)
	}

	gotParams := 0
	var sawKeyData, sawReady bool
	for !sawReady {
		typ, payload, err = client.ReadMessage()
		if err != nil {
			t.Fatalf("read message: %v", err)
		}
		switch typ {
		case MsgParamStatus:
			if _, _, err := DecodeParamStatus(payload); err != nil {
				t.Errorf("DecodeParamStatus: %v", err)
			}
			gotParams++
		case MsgBackendKey:
			pid, secret, err := DecodeBackendKeyData(payload)
			if err != nil {
				t.Errorf("DecodeBackendKeyData: %v", err)
			}
			if pid != 4242 || secret != 99 {
				t.Errorf("pid/secret = %d/%d, want 4242/99", pid, secret)
			}
			sawKeyData = true
		case MsgReadyForQuery:
			tx, err := DecodeReadyForQuery(payload)
			if err != nil {
				t.Errorf("DecodeReadyForQuery: %v", err)
			}
			if tx != TxIdle {
				t.Errorf("tx status = %q, want idle", tx)
			}
			if !sawKeyData {
				t.Error("ReadyForQuery arrived before BackendKeyData")
			}
			sawReady = true
		default:
			t.Fatalf("unexpected type %q", typ)
		}
	}
	if gotParams != 2 {
		t.Errorf("ParameterStatus count = %d, want 2", gotParams)
	}
	if err := <-serverErr; err != nil {
		t.Errorf("SendServerHandshake: %v", err)
	}
}

func TestReadStartupRejectsBadVersion(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	go func() {
		var body []byte
		body = binary.BigEndian.AppendUint32(body, 196609) // version 3.1
		body = append(body, "user\x00x\x00\x00"...)
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(body)+4))
		c.Write(hdr[:])
		c.Write(body)
	}()

	if _, _, err := NewFramer(s).ReadStartup(); !errors.Is(err, ErrBadVersion) {
		t.Errorf("err = %v, want ErrBadVersion", err)
	}
}

func TestCleartextPasswordExchange(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	client, server := NewFramer(c), NewFramer(s)

	go func() {
		_ = client.WritePassword("hunter2")
		_ = client.Flush()
	}()

	t2, payload, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if t2 != MsgPassword {
		t.Errorf("type = %q, want MsgPassword ('p')", t2)
	}
	pw, err := DecodePassword(payload)
	if err != nil {
		t.Fatalf("DecodePassword: %v", err)
	}
	if pw != "hunter2" {
		t.Errorf("password = %q, want %q", pw, "hunter2")
	}
}

func TestDecodeShortPayloads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fn   func() error
	}{
		{"auth", func() error { _, err := DecodeAuth([]byte{0, 0}); return err }},
		{"password", func() error { _, err := DecodePassword([]byte("no-null")); return err }},
		{"param", func() error { _, _, err := DecodeParamStatus([]byte("name")); return err }},
		{"backendkey", func() error { _, _, err := DecodeBackendKeyData([]byte{1, 2, 3}); return err }},
		{"ready", func() error { _, err := DecodeReadyForQuery(nil); return err }},
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

The handshake is correct when the message order is fixed, the version gate holds, and both `Authentication` messages are distinguished by code rather than type. `ReadStartup` reads the typeless frame and rejects anything but 3.0 with `ErrBadVersion`. The cleartext exchange round-trips: the server requests a password with `AuthenticationCleartextPassword`, the client answers with a `PasswordMessage`, and the server decodes it before sending `AuthenticationOk` and the rest of `SendServerHandshake`. `ParameterStatus`, `BackendKeyData`, and `ReadyForQuery` arrive in that order, and the client blocks on `ReadyForQuery` exactly as a real driver does. Every decoder rejects a truncated payload as `ErrShort`.

The mistake to avoid is treating the type byte `'R'` as enough to identify the message: the password request and the OK share it, and only the `int32` code separates them — branch on the code. The second is forgetting that the startup frame has no type byte, so the server's first read is `ReadStartup`, never `ReadMessage`.

## Resources

- [PostgreSQL: Protocol Flow — Start-Up](https://www.postgresql.org/docs/current/protocol-flow.html#PROTOCOL-FLOW-START-UP) — the startup and authentication sequence and the Authentication message codes.
- [PostgreSQL: Message Formats](https://www.postgresql.org/docs/current/protocol-message-formats.html) — StartupMessage, AuthenticationOk, AuthenticationCleartextPassword, PasswordMessage, ParameterStatus, BackendKeyData, ReadyForQuery.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `binary.BigEndian` and `AppendUint32`/`PutUint32`.
- [`net.Pipe`](https://pkg.go.dev/net#Pipe) — the in-memory connection the demo and tests use.

---

Back to [06-streaming-result-set.md](06-streaming-result-set.md) | Next: [../10-full-embedded-database/00-concepts.md](../10-full-embedded-database/00-concepts.md)
