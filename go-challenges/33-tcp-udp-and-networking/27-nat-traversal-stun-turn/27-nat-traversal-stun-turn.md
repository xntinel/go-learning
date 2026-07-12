# 27. NAT Traversal with STUN/TURN

Most devices sit behind a NAT that assigns them a private IP address invisible to
the public internet. Peer-to-peer applications — video calls, gaming, file sharing
— cannot connect directly to private addresses, so they need a way to discover and
punch through those NATs. This lesson builds three interacting pieces from scratch
using only the standard library: a STUN codec (RFC 5389 binary format), a STUN
server that reports each client's public address, and a STUN client that queries
it. The hard parts are the binary encoding rules (XOR masking, 4-byte alignment),
managing context-cancellable UDP servers correctly, and writing tests that exercise
the full request/response cycle on loopback sockets.

```text
stun/
  go.mod
  stun.go            -- RFC 5389 codec: constants, Message, Attribute, encode/decode
  server.go          -- UDP server: reads Binding Requests, writes Binding Responses
  client.go          -- UDP client: sends a Binding Request, reads XOR-MAPPED-ADDRESS
  stun_test.go       -- table-driven tests + Example with // Output:
  cmd/demo/main.go   -- runnable demo (go run ./cmd/demo)
```

## Concepts

### NAT and the Mapping Problem

Network Address Translation rewrites the source IP and port of outgoing packets
so that many private hosts can share one public IP. The NAT device maintains a
mapping table: when a packet leaves `192.168.1.5:52000` the NAT rewrites the
source to `203.0.113.1:41234` (the public IP with an assigned port). Return
traffic arriving at `203.0.113.1:41234` is rewritten back and forwarded to
`192.168.1.5:52000`. Crucially, neither side of an incoming connection knows
the public address of the private host — that information lives only in the NAT
device. A peer on the internet trying to initiate a connection to `192.168.1.5`
has no route.

Four NAT behaviors matter for traversal (RFC 4787):

- Full-cone (one-to-one): once a mapping exists, any external host can send
  packets to the mapped address.
- Address-restricted cone: return packets are accepted only from the external
  host that the local host previously sent a packet to.
- Port-restricted cone: like address-restricted, but the source port of the
  incoming packet must also match.
- Symmetric: a new mapping is created for each unique destination, so
  `host:port1` for peer A and `host:port2` for peer B even from the same local
  port. Hole punching fails against a symmetric NAT.

### STUN Message Format (RFC 5389)

A STUN message is a binary datagram with a 20-byte fixed header followed by
zero or more TLV attributes:

```text
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|0 0|     Message Type (14)     |       Message Length (16)     |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                     Magic Cookie = 0x2112A442                 |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                                                               |
|                    Transaction ID (96 bits)                   |
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

- The top two bits of the message type are always zero; this distinguishes STUN
  from other UDP traffic.
- Message Length does not include the 20-byte header.
- The Magic Cookie `0x2112A442` is fixed and lets a receiver quickly verify it
  is looking at a STUN message.
- The Transaction ID is a 96-bit (12-byte) random value matching a request to
  its response.

Each attribute is also TLV-encoded:

```text
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|              Attribute Type   |            Length             |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                        Value (variable) ...                   |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

The value is zero-padded to the next 4-byte boundary; the Length field records
the actual byte count before padding.

### XOR-MAPPED-ADDRESS: Why XOR?

A NAT device rewrites IP and port in IP and UDP headers but does not inspect
application-layer payloads. A naive STUN design would put the client's observed
public address directly in the payload. Some early NATs performed deep-packet
inspection on RTP/SIP and rewrote addresses they found in the body — breaking
STUN. RFC 5389 avoids this by XOR-masking the address before putting it on the
wire:

```text
XOR'd port = client_port XOR (magic_cookie >> 16)    -- i.e. XOR with 0x2112
XOR'd IPv4  = client_ip  XOR magic_cookie             -- i.e. XOR with 0x2112A442
```

The receiver simply XORs again to recover the original values. The masked bytes
do not look like IP addresses to a NAT's heuristic scanner.

### UDP Hole Punching

Once two peers know each other's public (STUN-mapped) addresses, they can
attempt to create NAT mappings that allow direct traffic:

1. Both peers simultaneously send UDP packets to the other's public address.
2. The outgoing packet from peer A creates a mapping in A's NAT: packets arriving
   at A's public port from B's public address are now forwarded to A's private
   address.
3. The same happens at B's NAT for A.
4. Packets from each peer arrive at the other's NAT just as the mapping is being
   created, and the connection is established.

The "simultaneous" constraint is why a signaling server is required: both sides
must start sending at roughly the same time, otherwise one peer's packet arrives
before the other's NAT has an outbound mapping and is dropped.

Hole punching fails against symmetric NAT because each outbound packet to a new
destination creates a different public port; the peer's guess of the public port
is wrong.

### TURN Relay: The Fallback

When hole punching fails, TURN (RFC 5766) provides a relay server with a public
address. Each peer asks the TURN server to allocate a relay address
(`XOR-RELAYED-ADDRESS`) for them. Packets sent to that relay address are
forwarded to the real peer. The round-trip cost is higher (all traffic flows
through the relay) but connectivity is guaranteed regardless of NAT type.

TURN uses the same binary format as STUN (TURN is a superset) but adds several
request types: ALLOCATE (0x0003), REFRESH (0x0004), SEND INDICATION (0x0016),
DATA INDICATION (0x0017), CREATE PERMISSION (0x0008), CHANNEL BIND (0x0009).

### ICE: The Complete Framework

Interactive Connectivity Establishment (RFC 8445) combines STUN and TURN into
a full candidate-gathering and connectivity-check protocol. Each peer gathers a
list of "candidates" — local addresses, server-reflexive addresses (from STUN),
and relayed addresses (from TURN) — and tries them in priority order. ICE is
the protocol used by WebRTC. This lesson implements the STUN/TURN building
blocks that ICE relies on.

## Exercises

This is a library, not a program: there is no `main`. You verify it with
`go test`.

### Exercise 1: STUN Message Codec

Create `stun.go`. This file defines the RFC 5389 binary format: constants,
the `Message` and `Attribute` types, and the encode/decode functions.

```go
package stun

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

const (
	magicCookie    uint32 = 0x2112A442
	headerSize            = 20
	attrHeaderSize        = 4

	// Message types (RFC 5389 §6)
	BindingRequest  uint16 = 0x0001
	BindingResponse uint16 = 0x0101
	BindingError    uint16 = 0x0111

	// Attribute types (RFC 5389 §15)
	AttrMappedAddress    uint16 = 0x0001
	AttrXORMappedAddress uint16 = 0x0020
	AttrErrorCode        uint16 = 0x0009
	AttrSoftware         uint16 = 0x8022

	familyIPv4 = 0x01
)

var (
	// ErrShortMessage is returned when the datagram is too short to hold a STUN header.
	ErrShortMessage = errors.New("stun: message too short")
	// ErrBadMagic is returned when the magic cookie field does not equal 0x2112A442.
	ErrBadMagic = errors.New("stun: invalid magic cookie")
	// ErrAttrTruncated is returned when an attribute's declared length exceeds the buffer.
	ErrAttrTruncated = errors.New("stun: attribute truncated")
	// ErrUnsupportedFamily is returned for non-IPv4 addresses in XOR-MAPPED-ADDRESS.
	ErrUnsupportedFamily = errors.New("stun: unsupported address family")
)

// Message is a STUN message as specified by RFC 5389.
type Message struct {
	Type          uint16
	TransactionID [12]byte
	Attrs         []Attribute
}

// Attribute is a single STUN TLV attribute.
type Attribute struct {
	Type  uint16
	Value []byte
}

// NewTransactionID returns a cryptographically random 96-bit transaction ID.
func NewTransactionID() ([12]byte, error) {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("stun: generate transaction id: %w", err)
	}
	return id, nil
}

// Encode serialises the message into its RFC 5389 wire format.
func (m *Message) Encode() []byte {
	body := encodeAttrs(m.Attrs)
	buf := make([]byte, headerSize+len(body))
	binary.BigEndian.PutUint16(buf[0:2], m.Type)
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(body)))
	binary.BigEndian.PutUint32(buf[4:8], magicCookie)
	copy(buf[8:20], m.TransactionID[:])
	copy(buf[20:], body)
	return buf
}

func encodeAttrs(attrs []Attribute) []byte {
	var out []byte
	for _, a := range attrs {
		padded := (len(a.Value) + 3) &^ 3
		hdr := make([]byte, attrHeaderSize)
		binary.BigEndian.PutUint16(hdr[0:2], a.Type)
		binary.BigEndian.PutUint16(hdr[2:4], uint16(len(a.Value)))
		out = append(out, hdr...)
		out = append(out, a.Value...)
		for i := len(a.Value); i < padded; i++ {
			out = append(out, 0)
		}
	}
	return out
}

// Decode parses a STUN message from raw bytes, returning ErrShortMessage or
// ErrBadMagic if the header is invalid.
func Decode(b []byte) (*Message, error) {
	if len(b) < headerSize {
		return nil, ErrShortMessage
	}
	if binary.BigEndian.Uint32(b[4:8]) != magicCookie {
		return nil, ErrBadMagic
	}
	m := &Message{
		Type: binary.BigEndian.Uint16(b[0:2]),
	}
	copy(m.TransactionID[:], b[8:20])
	bodyLen := int(binary.BigEndian.Uint16(b[2:4]))
	if len(b)-headerSize < bodyLen {
		return nil, ErrAttrTruncated
	}
	attrs, err := decodeAttrs(b[headerSize : headerSize+bodyLen])
	if err != nil {
		return nil, err
	}
	m.Attrs = attrs
	return m, nil
}

func decodeAttrs(b []byte) ([]Attribute, error) {
	var attrs []Attribute
	for len(b) >= attrHeaderSize {
		t := binary.BigEndian.Uint16(b[0:2])
		l := int(binary.BigEndian.Uint16(b[2:4]))
		b = b[attrHeaderSize:]
		if len(b) < l {
			return nil, ErrAttrTruncated
		}
		val := make([]byte, l)
		copy(val, b[:l])
		attrs = append(attrs, Attribute{Type: t, Value: val})
		padded := (l + 3) &^ 3
		if len(b) < padded {
			break
		}
		b = b[padded:]
	}
	return attrs, nil
}

// FindAttr returns the first attribute of the given type, or nil if absent.
func (m *Message) FindAttr(t uint16) *Attribute {
	for i := range m.Attrs {
		if m.Attrs[i].Type == t {
			return &m.Attrs[i]
		}
	}
	return nil
}

// EncodeXORMappedAddress encodes a UDP address into an XOR-MAPPED-ADDRESS
// attribute value per RFC 5389 §15.2.
//
// The port is XOR'd with the high 16 bits of the magic cookie (0x2112).
// The IPv4 address is XOR'd with the full magic cookie (0x2112A442).
func EncodeXORMappedAddress(addr *net.UDPAddr, _ [12]byte) ([]byte, error) {
	ip := addr.IP.To4()
	if ip == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFamily, addr.IP)
	}
	val := make([]byte, 8)
	val[0] = 0 // reserved
	val[1] = familyIPv4
	binary.BigEndian.PutUint16(val[2:4], uint16(addr.Port)^uint16(magicCookie>>16))
	mc := [4]byte{}
	binary.BigEndian.PutUint32(mc[:], magicCookie)
	val[4] = ip[0] ^ mc[0]
	val[5] = ip[1] ^ mc[1]
	val[6] = ip[2] ^ mc[2]
	val[7] = ip[3] ^ mc[3]
	return val, nil
}

// DecodeXORMappedAddress recovers the original UDP address from an
// XOR-MAPPED-ADDRESS attribute value.
func DecodeXORMappedAddress(val []byte, _ [12]byte) (*net.UDPAddr, error) {
	if len(val) < 8 {
		return nil, ErrAttrTruncated
	}
	if val[1] != familyIPv4 {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedFamily, val[1])
	}
	port := int(binary.BigEndian.Uint16(val[2:4]) ^ uint16(magicCookie>>16))
	mc := [4]byte{}
	binary.BigEndian.PutUint32(mc[:], magicCookie)
	ip := net.IP{val[4] ^ mc[0], val[5] ^ mc[1], val[6] ^ mc[2], val[7] ^ mc[3]}
	return &net.UDPAddr{IP: ip, Port: port}, nil
}
```

The `_` parameter in `EncodeXORMappedAddress` and `DecodeXORMappedAddress`
holds the transaction ID, which is required for XOR-MAPPED-ADDRESS with IPv6
(the transaction ID XORs with the upper 96 bits of the address). For IPv4, only
the magic cookie is needed; passing the transaction ID keeps the signature
forward-compatible with IPv6 support.

### Exercise 2: STUN Server

Create `server.go`. The server listens on a UDP port, reads Binding Requests,
and writes Binding Responses containing the client's observed source address in
`XOR-MAPPED-ADDRESS`.

```go
package stun

import (
	"context"
	"fmt"
	"log/slog"
	"net"
)

// Server is a minimal RFC 5389 STUN server.
type Server struct {
	conn   *net.UDPConn
	logger *slog.Logger
}

// NewServer creates a STUN server bound to addr (e.g. "0.0.0.0:3478" or
// "127.0.0.1:0" for an OS-assigned port in tests).
func NewServer(addr string, logger *slog.Logger) (*Server, error) {
	ua, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("stun: resolve %s: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp4", ua)
	if err != nil {
		return nil, fmt.Errorf("stun: listen %s: %w", addr, err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{conn: conn, logger: logger}, nil
}

// Addr returns the local address the server is bound to.
func (s *Server) Addr() *net.UDPAddr {
	return s.conn.LocalAddr().(*net.UDPAddr)
}

// Serve reads Binding Requests until ctx is cancelled, then returns ctx.Err().
//
// Serve closes the underlying UDP socket when ctx is cancelled so that
// blocked ReadFromUDP calls return immediately.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.conn.Close()
	}()
	buf := make([]byte, 1500)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return fmt.Errorf("stun: read: %w", err)
			}
		}
		if err := s.handle(buf[:n], from); err != nil {
			s.logger.Warn("stun handle error", "from", from, "err", err)
		}
	}
}

func (s *Server) handle(data []byte, from *net.UDPAddr) error {
	req, err := Decode(data)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if req.Type != BindingRequest {
		return fmt.Errorf("unexpected message type 0x%04x", req.Type)
	}
	xma, err := EncodeXORMappedAddress(from, req.TransactionID)
	if err != nil {
		return fmt.Errorf("encode XOR-MAPPED-ADDRESS: %w", err)
	}
	resp := &Message{
		Type:          BindingResponse,
		TransactionID: req.TransactionID,
		Attrs:         []Attribute{{Type: AttrXORMappedAddress, Value: xma}},
	}
	if _, err := s.conn.WriteToUDP(resp.Encode(), from); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}
```

Two design notes:

1. `Serve` closes the socket in a goroutine listening on `ctx.Done()`. This
   unblocks `ReadFromUDP` which otherwise ignores context. The subsequent select
   distinguishes a planned shutdown (return `ctx.Err()`) from a real I/O error.

2. The buffer is allocated once, outside the loop, because allocating a new
   `[]byte` per packet in a tight server loop creates significant GC pressure.

### Exercise 3: STUN Client

Create `client.go`. `Query` dials the server UDP address, sends a Binding
Request, and reads the `XOR-MAPPED-ADDRESS` from the Binding Response.

```go
package stun

import (
	"fmt"
	"net"
	"time"
)

const defaultTimeout = 3 * time.Second

// Query sends a STUN Binding Request to serverAddr and returns the
// public (as-seen-by-the-server) UDP address for this client.
//
// serverAddr must be a "host:port" string (e.g. "stun.l.google.com:19302").
func Query(serverAddr string) (*net.UDPAddr, error) {
	srv, err := net.ResolveUDPAddr("udp4", serverAddr)
	if err != nil {
		return nil, fmt.Errorf("stun: resolve %s: %w", serverAddr, err)
	}
	conn, err := net.DialUDP("udp4", nil, srv)
	if err != nil {
		return nil, fmt.Errorf("stun: dial: %w", err)
	}
	defer conn.Close()

	txID, err := NewTransactionID()
	if err != nil {
		return nil, err
	}
	req := &Message{Type: BindingRequest, TransactionID: txID}
	if _, err := conn.Write(req.Encode()); err != nil {
		return nil, fmt.Errorf("stun: write request: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(defaultTimeout)); err != nil {
		return nil, fmt.Errorf("stun: set deadline: %w", err)
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("stun: read response: %w", err)
	}
	resp, err := Decode(buf[:n])
	if err != nil {
		return nil, fmt.Errorf("stun: decode response: %w", err)
	}
	if resp.Type != BindingResponse {
		return nil, fmt.Errorf("stun: unexpected response type 0x%04x", resp.Type)
	}
	attr := resp.FindAttr(AttrXORMappedAddress)
	if attr == nil {
		return nil, fmt.Errorf("stun: missing XOR-MAPPED-ADDRESS in response")
	}
	return DecodeXORMappedAddress(attr.Value, resp.TransactionID)
}
```

### Exercise 4: Tests

Create `stun_test.go`. The tests are the verification — there is no `main` to
eyeball.

```go
package stun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestMessageRoundTrip verifies that Encode followed by Decode is an identity.
func TestMessageRoundTrip(t *testing.T) {
	t.Parallel()

	txID, err := NewTransactionID()
	if err != nil {
		t.Fatalf("NewTransactionID: %v", err)
	}
	original := &Message{Type: BindingRequest, TransactionID: txID}
	decoded, err := Decode(original.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Type != original.Type {
		t.Errorf("Type = 0x%04x, want 0x%04x", decoded.Type, original.Type)
	}
	if decoded.TransactionID != original.TransactionID {
		t.Error("TransactionID mismatch after round-trip")
	}
	if len(decoded.Attrs) != 0 {
		t.Errorf("Attrs len = %d, want 0", len(decoded.Attrs))
	}
}

func TestDecodeErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{
			name:    "too short",
			input:   []byte{0x00, 0x01},
			wantErr: ErrShortMessage,
		},
		{
			name:    "bad magic",
			input:   make([]byte, 20), // all zeros — magic is 0x00000000
			wantErr: ErrBadMagic,
		},
		{
			name: "attr truncated",
			// valid header + attr header claiming 10 bytes but body is 0 bytes
			input: func() []byte {
				m := &Message{Type: BindingRequest}
				// manually build a message with a truncated attr
				b := m.Encode()
				// attr header: type=0x0001, length=10 (but no following bytes)
				b = append(b, 0x00, 0x01, 0x00, 0x0a)
				// patch body length in header
				bodyLen := len(b) - headerSize
				b[2] = byte(bodyLen >> 8)
				b[3] = byte(bodyLen)
				return b
			}(),
			wantErr: ErrAttrTruncated,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Decode(tc.input)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Decode(%q) err = %v, want %v", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestXORMappedAddressRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ip   string
		port int
	}{
		{"127.0.0.1", 12345},
		{"192.168.1.100", 54321},
		{"10.0.0.1", 3478},
	}
	var txID [12]byte
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%s:%d", tc.ip, tc.port), func(t *testing.T) {
			t.Parallel()
			addr := &net.UDPAddr{IP: net.ParseIP(tc.ip).To4(), Port: tc.port}
			val, err := EncodeXORMappedAddress(addr, txID)
			if err != nil {
				t.Fatalf("EncodeXORMappedAddress: %v", err)
			}
			got, err := DecodeXORMappedAddress(val, txID)
			if err != nil {
				t.Fatalf("DecodeXORMappedAddress: %v", err)
			}
			if got.Port != tc.port {
				t.Errorf("port = %d, want %d", got.Port, tc.port)
			}
			if !got.IP.Equal(addr.IP) {
				t.Errorf("IP = %s, want %s", got.IP, addr.IP)
			}
		})
	}
}

func TestServerClientIntegration(t *testing.T) {
	t.Parallel()

	srv, err := NewServer("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

	mapped, err := Query(srv.Addr().String())
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !mapped.IP.IsLoopback() {
		t.Errorf("mapped IP = %s, want loopback", mapped.IP)
	}
	if mapped.Port == 0 {
		t.Error("mapped port = 0, want non-zero ephemeral port")
	}
}

// ExampleDecode demonstrates decoding a Binding Request and reading its type.
func ExampleDecode() {
	var txID [12]byte
	msg := &Message{Type: BindingRequest, TransactionID: txID}
	got, _ := Decode(msg.Encode())
	fmt.Printf("type=0x%04x attrs=%d\n", got.Type, len(got.Attrs))
	// Output: type=0x0001 attrs=0
}
```

Your turn: add `TestDecodeXORMappedAddressTruncated` that calls
`DecodeXORMappedAddress` with a 4-byte slice (shorter than the required 8 bytes)
and asserts `errors.Is(err, ErrAttrTruncated)`.

### Exercise 5: Runnable Demo

Create `cmd/demo/main.go`. Because `cmd/demo` is a separate `package main`, it
can only use exported API.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"time"

	"example.com/stun"
)

func main() {
	// Start a local STUN server on an OS-assigned port.
	srv, err := stun.NewServer("127.0.0.1:0", slog.Default())
	if err != nil {
		log.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

	addr := srv.Addr().String()
	fmt.Printf("STUN server listening on %s\n", addr)

	// Query the server from a client. The server observes the client's local
	// address (loopback here; a public STUN server would show the NAT address).
	mapped, err := stun.Query(addr)
	if err != nil {
		log.Fatalf("Query: %v", err)
	}
	fmt.Printf("Mapped address as seen by server: %s\n", mapped)

	// Demonstrate the codec independently.
	txID, err := stun.NewTransactionID()
	if err != nil {
		log.Fatalf("NewTransactionID: %v", err)
	}
	msg := &stun.Message{Type: stun.BindingRequest, TransactionID: txID}
	wire := msg.Encode()
	decoded, err := stun.Decode(wire)
	if err != nil {
		log.Fatalf("Decode: %v", err)
	}
	fmt.Printf("Round-trip: type=0x%04x txID match=%v wire-bytes=%d\n",
		decoded.Type, decoded.TransactionID == txID, len(wire))
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Wrong: XOR with the wrong constant

Wrong: XOR-ing the port with the full 32-bit magic cookie.

```go
// Wrong: 32-bit XOR applied to a 16-bit port — top bits are discarded
xorPort := uint32(port) ^ magicCookie
```

What happens: the port round-trips incorrectly. The decoded port is always
wrong. Tests that check port equality catch this immediately.

Fix: XOR the port with the high 16 bits only:

```go
// Correct per RFC 5389 §15.2
xorPort := uint16(port) ^ uint16(magicCookie>>16)
```

### Wrong: Forgetting 4-byte padding in attribute encoding

Wrong: writing the attribute value bytes without padding to a 4-byte boundary.

```go
// Wrong: attribute value written without padding
out = append(out, hdr...)
out = append(out, a.Value...)
// next attribute starts at an unaligned offset
```

What happens: the decoder reads the next attribute's type and length from the
wrong offset; `decodeAttrs` returns garbage or `ErrAttrTruncated`.

Fix: pad to the next multiple of 4 after each attribute value:

```go
padded := (len(a.Value) + 3) &^ 3
for i := len(a.Value); i < padded; i++ {
    out = append(out, 0)
}
```

### Wrong: Not distinguishing a planned shutdown from a real read error in Serve

Wrong: returning any `ReadFromUDP` error as a fatal error:

```go
n, from, err := s.conn.ReadFromUDP(buf)
if err != nil {
    return err // also returns when ctx is cancelled
}
```

What happens: every test that cancels a context reports a spurious error,
making it impossible to distinguish normal shutdown from real failures.

Fix: after a read error, check whether the context is done:

```go
if err != nil {
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
        return fmt.Errorf("stun: read: %w", err)
    }
}
```

### Wrong: Using `go test ./...` output as the only verification

Wrong: printing the mapped address with `fmt.Println` in `main` and manually
checking output.

What happens: the check is never automated; regressions go undetected.

Fix: the integration test `TestServerClientIntegration` runs under `go test
-race` and fails automatically if `Query` returns an error or the mapped address
is not loopback.

## Verification

From `~/go-exercises/stun`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must exit with status 0. `go test -race` is the verification
— there is no program to eyeball.

To run only the codec tests without network I/O:

```bash
go test -count=1 -race -run 'TestMessage|TestDecode|TestXOR' .
```

To run the full integration test including the loopback server:

```bash
go test -count=1 -race -run TestServerClientIntegration .
```

To try the demo:

```bash
go run ./cmd/demo
```

## Summary

- STUN is a binary UDP protocol (RFC 5389) with a 20-byte fixed header: 2-byte
  type, 2-byte body length, 4-byte magic cookie `0x2112A442`, 12-byte random
  transaction ID.
- Attributes are TLV-encoded and padded to 4-byte boundaries; the Length field
  records the true byte count before padding.
- XOR-MAPPED-ADDRESS masks the port with `0x2112` and the IPv4 address with
  `0x2112A442` to prevent legacy NATs from rewriting address bytes found inside
  application payloads.
- A server discovers a client's public address by reading the source IP and port
  from the UDP header of the incoming packet and encoding them in the response.
- UDP hole punching requires both peers to send packets simultaneously so that
  each creates a NAT mapping before the other's packet arrives.
- TURN provides a relay when hole punching fails (symmetric NAT); it adds
  ALLOCATE, REFRESH, and SEND INDICATION message types on top of the STUN codec.
- ICE (RFC 8445) combines STUN, TURN, and candidate gathering into the
  full framework used by WebRTC.
- Context cancellation in a UDP server must close the socket to unblock a
  blocked `ReadFromUDP`; the post-error `select` on `ctx.Done()` distinguishes
  planned shutdown from real I/O failures.

## What's Next

Next: [Packet Sniffer with BPF](../28-packet-sniffer-bpf/28-packet-sniffer-bpf.md).

## Resources

- [RFC 5389 -- Session Traversal Utilities for NAT (STUN)](https://datatracker.ietf.org/doc/html/rfc5389) -- binary format, XOR-MAPPED-ADDRESS, magic cookie
- [RFC 5766 -- Traversal Using Relays around NAT (TURN)](https://datatracker.ietf.org/doc/html/rfc5766) -- relay allocation, ALLOCATE/REFRESH/SEND message types
- [RFC 8445 -- Interactive Connectivity Establishment (ICE)](https://datatracker.ietf.org/doc/html/rfc8445) -- candidate gathering, connectivity checks, ICE roles
- [How NAT Traversal Works (Tailscale Blog)](https://tailscale.com/blog/how-nat-traversal-works/) -- practical NAT type taxonomy and hole-punching mechanics with timing diagrams
- [pkg.go.dev/net -- net.UDPConn](https://pkg.go.dev/net#UDPConn) -- ReadFromUDP, WriteToUDP, SetReadDeadline signatures
