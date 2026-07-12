# 19. Building a SOCKS5 Proxy

SOCKS5 (RFC 1928) operates at the transport layer: the proxy connects to a TCP
target on the client's behalf and relays raw bytes bidirectionally. Unlike HTTP
proxies, SOCKS5 is protocol-agnostic and can tunnel any TCP protocol — SSH,
database connections, SMTP. Three aspects make the implementation non-obvious:
(1) every fixed-length field in the binary wire format must be read with
`io.ReadFull`, not `io.Read`, because a partial read silently corrupts the
protocol state machine; (2) bidirectional relay needs half-close via
`(*net.TCPConn).CloseWrite` so that EOF in one direction does not prematurely
terminate the other; (3) hostnames in CONNECT requests are resolved on the proxy
side, which is the feature that gives SOCKS5 its DNS-privacy value.

## Concepts

### The Three-Phase Wire Format

SOCKS5 runs three sequential phases over a single TCP connection.

**Phase 1 — Method negotiation (RFC 1928 section 3).** The client sends VER
(0x05), NMETHODS (1 byte), and a list of supported auth method bytes. The server
picks one and writes VER + METHOD (2 bytes). If no method is acceptable the
server writes METHOD = 0xFF and closes.

**Phase 2 — Authentication.** For no-auth (method 0x00) this phase is skipped.
For username/password (method 0x02, RFC 1929), the client sends VER (0x01),
ULEN, UNAME, PLEN, PASSWD. The server responds with VER (0x01) and STATUS
(0x00 = success, non-zero = failure).

**Phase 3 — Command.** The client sends VER (0x05), CMD (0x01 for CONNECT), RSV
(0x00), ATYP, DST.ADDR, DST.PORT. The server dials the target and replies with
VER, REP, RSV, ATYP, BND.ADDR (4 bytes), BND.PORT (2 bytes). A REP of 0x00
means the target connection is open; raw bytes are now relayed in both
directions.

### Why io.ReadFull, Not io.Read

`net.Conn.Read` guarantees at least one byte per call but not a full buffer. A
TCP segment boundary, kernel scheduling, or a slow sender can split a logical
message across multiple `Read` calls. If you call `conn.Read(hdr)` expecting
4 bytes and only 2 arrive, the next byte is silently parsed as the wrong field
and the rest of the stream is permanently misaligned. `io.ReadFull(conn, buf)`
loops internally until exactly `len(buf)` bytes have been read or an error
occurs.

### Address Types and Proxy-Side Resolution

Three wire encodings for DST.ADDR:

- 0x01 (IPv4): 4 fixed bytes.
- 0x04 (IPv6): 16 fixed bytes.
- 0x03 (domain): 1-byte length prefix followed by the hostname bytes.

The domain case is the important one: the client sends the hostname as raw
bytes and the proxy resolves it with `net.Dial`. DNS stays outside the client's
network, which is the primary motivation for SOCKS5 in privacy-sensitive
deployments.

### Bidirectional Relay and Half-Close

After the CONNECT handshake succeeds, two goroutines each copy one direction.
When one direction reaches EOF (the sender closed its write side), a naive
implementation calls `conn.Close()`, terminating both directions. The correct
call is `(*net.TCPConn).CloseWrite()`, which sends a TCP FIN in that direction
while leaving the receive side open. The other goroutine continues until the
remote peer also closes.

Without half-close, protocols that rely on EOF signaling — many line-oriented
and request/response protocols — stall or corrupt their termination handshake.

### Reply Codes

| Code | Meaning                 |
|------|-------------------------|
| 0x00 | Success                 |
| 0x01 | General server failure  |
| 0x03 | Network unreachable     |
| 0x04 | Host unreachable        |
| 0x05 | Connection refused      |
| 0x07 | Command not supported   |

The proxy maps a `net.Dial` error to one of these codes before the relay
starts. Once the relay is running, errors cannot be reported via SOCKS5 — bytes
are forwarded until both directions close.

## Exercises

This is a library with a runnable demo. Verification is with `go test`.

### Exercise 1: Server Type, Constants, and Core Protocol

Create `socks5.go`:

```go
package socks5

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// SOCKS5 protocol constants (RFC 1928, RFC 1929).
const (
	version5 byte = 0x05

	MethodNoAuth       byte = 0x00
	MethodUserPassword byte = 0x02
	MethodNoAcceptable byte = 0xFF

	CmdConnect byte = 0x01

	AddrIPv4   byte = 0x01
	AddrDomain byte = 0x03
	AddrIPv6   byte = 0x04

	ReplySuccess         byte = 0x00
	ReplyGeneralFailure  byte = 0x01
	ReplyNetUnreachable  byte = 0x03
	ReplyHostUnreachable byte = 0x04
	ReplyConnRefused     byte = 0x05
	ReplyCmdNotSupported byte = 0x07
)

var (
	// ErrAuthFailed is returned when username/password credentials are wrong.
	ErrAuthFailed = errors.New("socks5: authentication failed")
	// ErrNoAcceptable is returned when the client offers no method the server supports.
	ErrNoAcceptable = errors.New("socks5: no acceptable auth method")
	// ErrUnsupportedCmd is returned for SOCKS5 commands other than CONNECT.
	ErrUnsupportedCmd = errors.New("socks5: unsupported command")
	// ErrBadVersion is returned when the client sends a version other than 0x05.
	ErrBadVersion = errors.New("socks5: bad SOCKS version")
)

// Credentials maps usernames to passwords. A nil Credentials selects no-auth
// mode. A non-nil map enables username/password authentication (RFC 1929).
type Credentials map[string]string

// Server is a concurrent SOCKS5 proxy. It supports the CONNECT command with
// no-auth and username/password authentication.
type Server struct {
	addr        string
	credentials Credentials

	activeConns atomic.Int64
	totalConns  atomic.Int64
}

// New creates a Server. addr is the TCP listen address (e.g., ":1080").
// Pass nil creds for no-auth mode.
func New(addr string, creds Credentials) *Server {
	return &Server{addr: addr, credentials: creds}
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.addr }

// RequiresAuth reports whether the server demands username/password auth.
func (s *Server) RequiresAuth() bool { return s.credentials != nil }

// ActiveConns returns the number of currently active proxy connections.
func (s *Server) ActiveConns() int64 { return s.activeConns.Load() }

// TotalConns returns the total connections handled since startup.
func (s *Server) TotalConns() int64 { return s.totalConns.Load() }

// ListenAndServe starts the SOCKS5 proxy on the configured address.
// It blocks until the listener closes.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("socks5: listen: %w", err)
	}
	defer ln.Close()
	return s.Serve(ln)
}

// Serve accepts connections from ln and handles each concurrently.
// It returns the first Accept error, usually from ln.Close().
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		s.totalConns.Add(1)
		s.activeConns.Add(1)
		go func(c net.Conn) {
			defer s.activeConns.Add(-1)
			s.handle(c)
		}(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	dest, err := s.handshake(conn)
	if err != nil {
		return
	}
	relay(conn, dest)
}

// handshake runs all three SOCKS5 phases and returns the open connection to
// the target host on success.
func (s *Server) handshake(conn net.Conn) (net.Conn, error) {
	method, err := s.negotiateMethod(conn)
	if err != nil {
		return nil, err
	}
	if method == MethodUserPassword {
		if err := s.authenticate(conn); err != nil {
			return nil, err
		}
	}
	return s.doConnect(conn)
}

// negotiateMethod reads the client method list and writes the server choice.
func (s *Server) negotiateMethod(conn net.Conn) (byte, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return 0, err
	}
	if hdr[0] != version5 {
		return 0, fmt.Errorf("%w: %d", ErrBadVersion, hdr[0])
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return 0, err
	}

	chosen := MethodNoAcceptable
	if s.credentials == nil {
		for _, m := range methods {
			if m == MethodNoAuth {
				chosen = MethodNoAuth
				break
			}
		}
	} else {
		for _, m := range methods {
			if m == MethodUserPassword {
				chosen = MethodUserPassword
				break
			}
		}
	}

	if _, err := conn.Write([]byte{version5, chosen}); err != nil {
		return 0, err
	}
	if chosen == MethodNoAcceptable {
		return 0, ErrNoAcceptable
	}
	return chosen, nil
}

// authenticate performs the RFC 1929 username/password sub-negotiation.
func (s *Server) authenticate(conn net.Conn) error {
	verBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, verBuf); err != nil {
		return err
	}

	ulenBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, ulenBuf); err != nil {
		return err
	}
	uname := make([]byte, ulenBuf[0])
	if _, err := io.ReadFull(conn, uname); err != nil {
		return err
	}

	plenBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, plenBuf); err != nil {
		return err
	}
	passwd := make([]byte, plenBuf[0])
	if _, err := io.ReadFull(conn, passwd); err != nil {
		return err
	}

	want, ok := s.credentials[string(uname)]
	if !ok || want != string(passwd) {
		conn.Write([]byte{0x01, 0x01}) // sub-negotiation failure; error not actionable
		return ErrAuthFailed
	}
	_, err := conn.Write([]byte{0x01, 0x00}) // success
	return err
}

// doConnect reads the CONNECT request, dials the target, and sends the reply.
func (s *Server) doConnect(conn net.Conn) (net.Conn, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, err
	}
	if hdr[0] != version5 {
		return nil, fmt.Errorf("%w: %d", ErrBadVersion, hdr[0])
	}
	if hdr[1] != CmdConnect {
		sendReply(conn, ReplyCmdNotSupported, nil)
		return nil, fmt.Errorf("%w: %#x", ErrUnsupportedCmd, hdr[1])
	}

	target, err := readAddr(conn, hdr[3])
	if err != nil {
		sendReply(conn, ReplyGeneralFailure, nil)
		return nil, err
	}

	dest, err := net.Dial("tcp", target)
	if err != nil {
		sendReply(conn, dialErrReply(err), nil)
		return nil, err
	}

	local, _ := dest.LocalAddr().(*net.TCPAddr)
	if err := sendReply(conn, ReplySuccess, local); err != nil {
		dest.Close()
		return nil, err
	}
	return dest, nil
}

// readAddr reads the ATYP-specific address bytes and the 2-byte big-endian
// port. It returns "host:port" ready for net.Dial.
func readAddr(r io.Reader, atyp byte) (string, error) {
	var host string
	switch atyp {
	case AddrIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case AddrIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case AddrDomain:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return "", err
		}
		name := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(r, name); err != nil {
			return "", err
		}
		host = string(name)
	default:
		return "", fmt.Errorf("socks5: unsupported address type %#x", atyp)
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(r, portBuf); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(portBuf)
	return fmt.Sprintf("%s:%d", host, port), nil
}

// sendReply writes a 10-byte SOCKS5 reply to conn. bound is the proxy's local
// address toward the target; pass nil for error replies.
func sendReply(conn net.Conn, rep byte, bound *net.TCPAddr) error {
	var ip [4]byte
	var port uint16
	if bound != nil {
		if v4 := bound.IP.To4(); v4 != nil {
			copy(ip[:], v4)
		}
		port = uint16(bound.Port)
	}
	buf := [10]byte{
		version5, rep, 0x00, AddrIPv4,
		ip[0], ip[1], ip[2], ip[3],
		byte(port >> 8), byte(port),
	}
	_, err := conn.Write(buf[:])
	return err
}

// relay copies data between client and dest bidirectionally.
// It calls CloseWrite so that EOF in one direction does not block the other.
func relay(client, dest net.Conn) {
	defer dest.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}

	go copyHalf(dest, client)
	go copyHalf(client, dest)
	wg.Wait()
}

// dialErrReply maps a net.Dial error to a SOCKS5 reply code.
func dialErrReply(err error) byte {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Timeout() {
		return ReplyHostUnreachable
	}
	return ReplyConnRefused
}
```

Defaults are applied once in `New`; option validation is done per-connection.
The `negotiateMethod`, `authenticate`, and `doConnect` chain runs sequentially
on the goroutine handling each connection.

### Exercise 2: Tests and the Example Function

Create `socks5_test.go`:

```go
package socks5

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

func ExampleNew() {
	noAuth := New(":1080", nil)
	withAuth := New(":1080", Credentials{"alice": "secret"})
	fmt.Println(noAuth.RequiresAuth())
	fmt.Println(withAuth.RequiresAuth())
	// Output:
	// false
	// true
}

func TestNegotiateMethodNoAuth(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	srv := New("", nil)
	errCh := make(chan error, 1)
	go func() {
		method, err := srv.negotiateMethod(s)
		if err == nil && method != MethodNoAuth {
			err = fmt.Errorf("method = %d, want %d", method, MethodNoAuth)
		}
		errCh <- err
	}()

	// v5, 1 method, no-auth.
	if _, err := c.Write([]byte{0x05, 0x01, MethodNoAuth}); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatal(err)
	}
	if resp[0] != version5 || resp[1] != MethodNoAuth {
		t.Fatalf("method response = %v, want [5 0]", resp)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestNegotiateMethodUserPassword(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	srv := New("", Credentials{"u": "p"})
	errCh := make(chan error, 1)
	go func() {
		method, err := srv.negotiateMethod(s)
		if err == nil && method != MethodUserPassword {
			err = fmt.Errorf("method = %d, want %d", method, MethodUserPassword)
		}
		errCh <- err
	}()

	// Offer both no-auth and user/pass; server must pick user/pass.
	if _, err := c.Write([]byte{0x05, 0x02, MethodNoAuth, MethodUserPassword}); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatal(err)
	}
	if resp[1] != MethodUserPassword {
		t.Fatalf("method = %d, want %d (user/pass)", resp[1], MethodUserPassword)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestNegotiateMethodNoAcceptable(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	// Server requires user/pass but client only offers no-auth.
	srv := New("", Credentials{"u": "p"})
	errCh := make(chan error, 1)
	go func() {
		_, err := srv.negotiateMethod(s)
		errCh <- err
	}()

	c.Write([]byte{0x05, 0x01, MethodNoAuth})
	resp := make([]byte, 2)
	io.ReadFull(c, resp)
	if resp[1] != MethodNoAcceptable {
		t.Fatalf("wanted 0xFF (no acceptable), got %#x", resp[1])
	}
	if err := <-errCh; !errors.Is(err, ErrNoAcceptable) {
		t.Fatalf("err = %v, want ErrNoAcceptable", err)
	}
}

func TestAuthenticateSuccess(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	srv := New("", Credentials{"alice": "secret"})
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.authenticate(s)
	}()

	uname := []byte("alice")
	passwd := []byte("secret")
	msg := append([]byte{0x01, byte(len(uname))}, uname...)
	msg = append(msg, byte(len(passwd)))
	msg = append(msg, passwd...)
	c.Write(msg)

	authResp := make([]byte, 2)
	io.ReadFull(c, authResp)
	if authResp[1] != 0x00 {
		t.Fatalf("auth status = %d, want 0 (success)", authResp[1])
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateFail(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	srv := New("", Credentials{"alice": "correct"})
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.authenticate(s)
	}()

	uname := []byte("alice")
	passwd := []byte("wrong")
	msg := append([]byte{0x01, byte(len(uname))}, uname...)
	msg = append(msg, byte(len(passwd)))
	msg = append(msg, passwd...)
	c.Write(msg)

	authResp := make([]byte, 2)
	io.ReadFull(c, authResp)
	if authResp[1] == 0x00 {
		t.Fatal("wrong password accepted as success")
	}
	if err := <-errCh; !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

// startEcho starts a local TCP echo server and returns its address.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c)
		}
	}()
	return ln.Addr().String()
}

// startProxy starts a SOCKS5 proxy and returns its address.
func startProxy(t *testing.T, creds Credentials) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := New("", creds)
	go srv.Serve(ln)
	return ln.Addr().String()
}

// mustPort parses the port from "host:port".
func mustPort(t *testing.T, addr string) uint16 {
	t.Helper()
	ta, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatalf("mustPort: %v", err)
	}
	return uint16(ta.Port)
}

func TestConnectAndRelayNoAuth(t *testing.T) {
	t.Parallel()

	echoPort := mustPort(t, startEcho(t))
	proxyAddr := startProxy(t, nil)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Phase 1: no-auth.
	conn.Write([]byte{0x05, 0x01, MethodNoAuth})
	resp := make([]byte, 2)
	io.ReadFull(conn, resp)
	if resp[1] != MethodNoAuth {
		t.Fatalf("method = %d, want no-auth (0)", resp[1])
	}

	// Phase 3: CONNECT to echo server via IPv4.
	req := []byte{
		0x05, CmdConnect, 0x00, AddrIPv4,
		127, 0, 0, 1,
		byte(echoPort >> 8), byte(echoPort),
	}
	conn.Write(req)
	reply := make([]byte, 10)
	io.ReadFull(conn, reply)
	if reply[1] != ReplySuccess {
		t.Fatalf("CONNECT reply = %d, want 0 (success)", reply[1])
	}

	// Relay: send data and verify echo.
	msg := []byte("hello socks5")
	conn.Write(msg)
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo = %q, want %q", buf, msg)
	}
}

func TestConnectAndRelayWithAuth(t *testing.T) {
	t.Parallel()

	echoPort := mustPort(t, startEcho(t))
	proxyAddr := startProxy(t, Credentials{"admin": "pass"})

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Phase 1: offer user/pass.
	conn.Write([]byte{0x05, 0x01, MethodUserPassword})
	resp := make([]byte, 2)
	io.ReadFull(conn, resp)
	if resp[1] != MethodUserPassword {
		t.Fatalf("method = %d, want user/pass (2)", resp[1])
	}

	// Phase 2: authenticate.
	uname, passwd := []byte("admin"), []byte("pass")
	auth := append([]byte{0x01, byte(len(uname))}, uname...)
	auth = append(auth, byte(len(passwd)))
	auth = append(auth, passwd...)
	conn.Write(auth)
	authResp := make([]byte, 2)
	io.ReadFull(conn, authResp)
	if authResp[1] != 0x00 {
		t.Fatalf("auth status = %d, want 0 (success)", authResp[1])
	}

	// Phase 3: CONNECT and relay.
	req := []byte{
		0x05, CmdConnect, 0x00, AddrIPv4,
		127, 0, 0, 1,
		byte(echoPort >> 8), byte(echoPort),
	}
	conn.Write(req)
	reply := make([]byte, 10)
	io.ReadFull(conn, reply)
	if reply[1] != ReplySuccess {
		t.Fatalf("CONNECT reply = %d, want 0", reply[1])
	}

	msg := []byte("authenticated relay works")
	conn.Write(msg)
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo = %q, want %q", buf, msg)
	}
}

func TestConnectDomainAddr(t *testing.T) {
	t.Parallel()

	echoPort := mustPort(t, startEcho(t))
	proxyAddr := startProxy(t, nil)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.Write([]byte{0x05, 0x01, MethodNoAuth})
	resp := make([]byte, 2)
	io.ReadFull(conn, resp)

	// CONNECT using domain address type; proxy resolves the name.
	host := []byte("127.0.0.1")
	req := append([]byte{0x05, CmdConnect, 0x00, AddrDomain, byte(len(host))}, host...)
	req = append(req, byte(echoPort>>8), byte(echoPort))
	conn.Write(req)
	reply := make([]byte, 10)
	io.ReadFull(conn, reply)
	if reply[1] != ReplySuccess {
		t.Fatalf("CONNECT (domain) reply = %d, want 0", reply[1])
	}

	msg := []byte("domain addr relay")
	conn.Write(msg)
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo = %q, want %q", buf, msg)
	}
}

func TestBadCredentials(t *testing.T) {
	t.Parallel()

	proxyAddr := startProxy(t, Credentials{"user": "correct"})

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.Write([]byte{0x05, 0x01, MethodUserPassword})
	resp := make([]byte, 2)
	io.ReadFull(conn, resp)

	uname, passwd := []byte("user"), []byte("wrong")
	auth := append([]byte{0x01, byte(len(uname))}, uname...)
	auth = append(auth, byte(len(passwd)))
	auth = append(auth, passwd...)
	conn.Write(auth)
	authResp := make([]byte, 2)
	io.ReadFull(conn, authResp)
	if authResp[1] == 0x00 {
		t.Fatal("wrong credentials accepted as success")
	}
}

// Your turn: add TestConnectRefused that connects through the proxy to
// 127.0.0.1:1 (a port with nothing listening) and asserts reply[1] equals
// ReplyConnRefused (0x05).
```

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"example.com/socks5"
)

func main() {
	addr := ":1080"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	// No-auth for the demo. Supply a Credentials map to require login.
	srv := socks5.New(addr, nil)
	fmt.Printf("SOCKS5 proxy on %s  auth=%v\n", srv.Addr(), srv.RequiresAuth())
	fmt.Println("test: curl --socks5 127.0.0.1:1080 http://example.com")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		log.Fatal(err)
	case <-quit:
		fmt.Printf("\nshutdown - total conns: %d\n", srv.TotalConns())
	}
}
```

Run it with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Using io.Read Instead of io.ReadFull

Wrong: `conn.Read(hdr)` where `hdr` is a 4-byte slice. If the peer sends the 4
bytes in two TCP segments, `Read` returns after 2 bytes. `hdr[2]` is zero
(uninitialized), which is parsed as the wrong ATYP byte, corrupting the rest of
the stream.

Fix: `io.ReadFull(conn, hdr)` loops until all 4 bytes arrive or an error occurs.
Every fixed-length field in SOCKS5 must use `ReadFull`.

### Closing the Whole Connection on Half-EOF

Wrong: when `io.Copy(dest, client)` returns (client closed its write side),
calling `dest.Close()` shuts down the write direction of dest AND closes the
read side, cutting off any data already in flight from the destination back to
the client.

Fix: call `dest.(*net.TCPConn).CloseWrite()`. This sends a TCP FIN to the
destination, signaling EOF for its read side, while leaving the proxy's read
side of `dest` open for the destination's remaining reply data.

### Resolving Hostnames on the Client Before Sending

Wrong: the client resolves `api.example.com` to `93.184.216.34` and sends an
IPv4 CONNECT request. The proxy has no idea a domain name was involved.

Fix: the client sends a domain CONNECT request (ATYP=0x03) with the raw
hostname bytes. The proxy calls `net.Dial("tcp", "api.example.com:443")`, and
DNS resolution happens on the proxy's network. This is the DNS-privacy guarantee
SOCKS5 is designed to provide.

### Ignoring the RSV Byte

Wrong: reading only 3 bytes of the CONNECT header (VER, CMD, ATYP), then
treating the next byte as the start of DST.ADDR. The RSV byte (always 0x00) is
silently consumed, misaligning the address parse.

Fix: read 4 bytes into `hdr := make([]byte, 4)`. The ATYP is `hdr[3]`, not
`hdr[2]`.

### Big-Endian Port Encoding

Wrong: writing the port as little-endian (`byte(port)`, `byte(port>>8)`).
A proxy running on a little-endian machine encodes port 80 as `[80, 0]` instead
of `[0, 80]`.

Fix: the SOCKS5 spec requires network byte order (big-endian).
`binary.BigEndian.Uint16(buf)` decodes it correctly on any architecture.

## Verification

From `~/go-exercises/socks5`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must succeed. The `-race` flag catches data races in the relay
goroutines. Add `TestConnectRefused` (described at the end of the test file) to
complete the error-path coverage.

## Summary

- SOCKS5 runs three sequential phases: method negotiation, optional
  authentication, and command execution.
- Every fixed-length read must use `io.ReadFull`; partial reads corrupt the
  state machine.
- The CONNECT command causes the proxy to dial the target; the connection
  becomes a raw byte relay after a success reply.
- Domain-name CONNECT requests move DNS resolution to the proxy's network, which
  is SOCKS5's core privacy feature.
- Half-close (`CloseWrite`) lets each direction of a relay reach EOF
  independently; using `Close` instead causes premature termination.
- Reply codes propagate specific dial errors (refused, unreachable) back to the
  client before the relay starts.

## What's Next

Next: [Custom Wire Protocol](../20-custom-wire-protocol/20-custom-wire-protocol.md).

## Resources

- [RFC 1928 - SOCKS Protocol Version 5](https://datatracker.ietf.org/doc/html/rfc1928)
- [RFC 1929 - Username/Password Authentication for SOCKS V5](https://datatracker.ietf.org/doc/html/rfc1929)
- [pkg.go.dev/io - ReadFull](https://pkg.go.dev/io#ReadFull)
- [pkg.go.dev/net - TCPConn.CloseWrite](https://pkg.go.dev/net#TCPConn.CloseWrite)
- [pkg.go.dev/net - Dial](https://pkg.go.dev/net#Dial)
