# 9. Custom Network Protocol Stack

Building a userspace TCP/IP stack means replacing the kernel's networking subsystem for one virtual interface. Every byte that a standard `ping` or `curl` delivers to your program is a raw Ethernet frame: no socket abstraction, no OS parsing, no automatic ACK generation. You must implement each protocol layer as an explicit state machine, get the byte order right in every header field, compute the RFC 1071 one's-complement checksum for both IPv4 and TCP, and run the full RFC 793 TCP state machine to complete a real three-way handshake with a standard Linux networking stack.

The core difficulty is not the individual parsers; it is the interaction between the layers. An ICMP echo reply must be encapsulated in an IPv4 packet with a correct header checksum that is itself encapsulated in an Ethernet frame addressed to the ARP-resolved MAC of the sender. Every field that is wrong causes silent packet loss.

Module layout:

```text
netstack/
  go.mod
  netstack.go       Ethernet, ARP, IPv4, ICMP types and parsing
  checksum.go       RFC 1071 Internet Checksum and TCP pseudo-header checksum
  tcp.go            TCP header, state machine (Conn), sliding window
  tap_linux.go      TUN/TAP device open + read/write loop (Linux only)
  netstack_test.go  table-driven tests for all pure-Go layers
  cmd/demo/main.go  runnable demo (no TAP device required)
```

## Concepts

### The Linux TAP Device

The kernel's `tun/tap` module exposes `/dev/net/tun`. A process opens it, assigns a name (`tap0`), and receives a file descriptor over which it reads and writes complete Ethernet II frames. The key `ioctl` is:

```
ioctl(fd, TUNSETIFF, &ifr)   // ifr.Flags = IFF_TAP | IFF_NO_PI
```

`IFF_TAP` selects Ethernet frames (as opposed to `IFF_TUN` for IP datagrams). `IFF_NO_PI` suppresses the four-byte "packet info" header the kernel otherwise prepends to each frame. Without `IFF_NO_PI` every read delivers four extra bytes and every write must pad accordingly.

After opening the device, configure the host-side peer IP and bring the interface up:

```bash
sudo ip addr add 10.0.0.1/24 dev tap0
sudo ip link set tap0 up
```

Your stack claims `10.0.0.2`. Traffic from the host to `10.0.0.2` is delivered to your program as raw Ethernet frames; your program writes replies back through the same descriptor.

### Protocol Layering and Dispatch

Each layer strips its own header and dispatches on a type/protocol field:

```
EtherFrame.EtherType 0x0806 → ARP handler
EtherFrame.EtherType 0x0800 → IPv4 → Protocol 1 → ICMP
                                    → Protocol 6 → TCP
```

The dispatch loop reads one frame at a time from the TAP descriptor and calls the appropriate handler. Because each handler may need to send one or more reply frames, the loop passes a write function (or channel) downward. Keep the dispatch loop simple: parse, dispatch, write—no blocking I/O inside a handler.

### The Internet Checksum (RFC 1071)

IPv4, ICMP, and TCP all use the same one's-complement checksum algorithm. Sum all 16-bit words of the data in a 32-bit accumulator, fold the carry bits back into 16 bits by adding the high 16 bits to the low 16 bits (repeat until no carry), then take the bitwise complement:

```
for sum>>16 != 0 { sum = (sum & 0xffff) + (sum >> 16) }
return ^uint16(sum)
```

To verify a received header: run the same algorithm over the entire header including the checksum field. The result must be 0x0000. This property holds because the one's-complement sum of a value and its own complement is 0xffff, which folds to 0x0000.

TCP uses a "pseudo-header" (RFC 793 §3.1) prepended before the TCP header and payload: source IP (4 bytes), destination IP (4 bytes), zero byte, protocol byte (6), and TCP segment length (2 bytes). The checksum covers the pseudo-header plus the segment; the pseudo-header is never transmitted.

### The TCP State Machine (RFC 793 §3.2)

TCP defines eleven states: CLOSED, LISTEN, SYN_SENT, SYN_RECEIVED, ESTABLISHED, FIN_WAIT_1, FIN_WAIT_2, CLOSE_WAIT, CLOSING, LAST_ACK, and TIME_WAIT. State transitions are triggered by incoming segments (SYN, SYN-ACK, ACK, FIN, RST) and by local application events (passive open, active open, close, send).

For a passive open (server accepting connections):

```
LISTEN  --[recv SYN]-->  SYN_RECEIVED  (send SYN-ACK)
SYN_RECEIVED  --[recv ACK]-->  ESTABLISHED
ESTABLISHED   --[recv FIN]-->  CLOSE_WAIT  (send ACK)
CLOSE_WAIT    --[app close]-->  LAST_ACK  (send FIN)
LAST_ACK      --[recv ACK]-->  CLOSED
```

Each `Conn` protects its state and sequence variables with a mutex so the receive loop and any concurrent send path do not race.

### Sequence Numbers and the Sliding Window

RFC 793 §3.2 defines the send-side variables as:

- `SND.UNA` — oldest unacknowledged sequence number
- `SND.NXT` — next sequence number to transmit
- `SND.WND` — send window (peer's advertised receive window)

The sender may only transmit data in the range `[SND.NXT, SND.UNA + SND.WND)`. The amount available to send at any moment is `SND.WND - (SND.NXT - SND.UNA)`. When the peer acknowledges data, `SND.UNA` advances and the window opens again.

SYN and FIN each consume one sequence number even though they carry no payload. After the three-way handshake, `SND.NXT = ISS + 1` where `ISS` is the initial send sequence number chosen at random. Sequence numbers wrap around at `2^32`; comparisons must use modular arithmetic.

### Retransmission: Exponential Backoff and Karn's Algorithm

Every transmitted segment that is not yet acknowledged must be on a retransmission queue with an associated timer. If the timer fires before an ACK arrives, retransmit the segment and double the timeout (exponential backoff with a maximum, typically 64 seconds). Reset the timer to the base RTO on receiving a new (non-duplicate) ACK.

Karn's algorithm: do not update the smoothed RTT estimate when retransmitting a segment, because it is ambiguous whether the ACK being received acknowledges the original transmission or the retransmission. Only update RTT samples from segments that were sent exactly once.

## Exercises

This is a library with a demo binary. Verify with `go test`, not `go run`.

### Exercise 1: Protocol Types, Parsing, and Serialization

Create `netstack.go`. Every parser returns a typed error wrapping `ErrFrameTooShort` so callers can use `errors.Is` without matching strings.

```go
// Package netstack provides pure-Go types for parsing and building Ethernet,
// ARP, IPv4, ICMP, and TCP protocol data units.  The TAP device integration
// and the full runtime loop are in tap_linux.go (Linux only).
package netstack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// EtherType values used for upper-layer dispatch.
const (
	EtherTypeIPv4 = 0x0800
	EtherTypeARP  = 0x0806
)

// IP protocol numbers.
const (
	ProtoICMP = 1
	ProtoTCP  = 6
)

// ICMP type codes.
const (
	ICMPTypeEchoReply   = 0
	ICMPTypeEchoRequest = 8
)

// ARP operation codes.
const (
	ARPRequest = 1
	ARPReply   = 2
)

// Sentinel errors.
var (
	ErrFrameTooShort = errors.New("netstack: frame too short")
	ErrChecksumFail  = errors.New("netstack: checksum mismatch")
	ErrUnknownProto  = errors.New("netstack: unknown protocol")
)

// HardwareAddr is a 6-byte Ethernet MAC address stored in network byte order.
type HardwareAddr [6]byte

// String returns the MAC address in the standard colon-hex notation.
func (h HardwareAddr) String() string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		h[0], h[1], h[2], h[3], h[4], h[5])
}

// BroadcastMAC is the Ethernet broadcast address ff:ff:ff:ff:ff:ff.
var BroadcastMAC = HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// EtherFrame is a parsed Ethernet II frame.
type EtherFrame struct {
	Dst       HardwareAddr
	Src       HardwareAddr
	EtherType uint16
	Payload   []byte
}

// ParseEtherFrame decodes an Ethernet II frame from raw bytes.
// The frame must be at least 14 bytes (the Ethernet header; payload may be empty).
func ParseEtherFrame(b []byte) (EtherFrame, error) {
	if len(b) < 14 {
		return EtherFrame{}, fmt.Errorf("%w: Ethernet header is 14 bytes, got %d", ErrFrameTooShort, len(b))
	}
	var f EtherFrame
	copy(f.Dst[:], b[0:6])
	copy(f.Src[:], b[6:12])
	f.EtherType = binary.BigEndian.Uint16(b[12:14])
	f.Payload = b[14:]
	return f, nil
}

// Marshal serializes the frame to a byte slice in network byte order.
func (f EtherFrame) Marshal() []byte {
	out := make([]byte, 14+len(f.Payload))
	copy(out[0:6], f.Dst[:])
	copy(out[6:12], f.Src[:])
	binary.BigEndian.PutUint16(out[12:14], f.EtherType)
	copy(out[14:], f.Payload)
	return out
}

// ARPPacket is an Ethernet/IPv4 ARP packet (hardware type 1, protocol 0x0800).
// The 8-byte fixed prefix (htype, ptype, hlen, plen) is implied.
type ARPPacket struct {
	Operation uint16 // ARPRequest or ARPReply
	SenderMAC HardwareAddr
	SenderIP  [4]byte
	TargetMAC HardwareAddr
	TargetIP  [4]byte
}

// ParseARPPacket decodes an ARP packet from an Ethernet payload.
// An Ethernet/IPv4 ARP packet is exactly 28 bytes.
func ParseARPPacket(b []byte) (ARPPacket, error) {
	if len(b) < 28 {
		return ARPPacket{}, fmt.Errorf("%w: ARP packet is 28 bytes, got %d", ErrFrameTooShort, len(b))
	}
	var p ARPPacket
	p.Operation = binary.BigEndian.Uint16(b[6:8])
	copy(p.SenderMAC[:], b[8:14])
	copy(p.SenderIP[:], b[14:18])
	copy(p.TargetMAC[:], b[18:24])
	copy(p.TargetIP[:], b[24:28])
	return p, nil
}

// Marshal serializes the ARP packet to 28 bytes.
func (p ARPPacket) Marshal() []byte {
	b := make([]byte, 28)
	binary.BigEndian.PutUint16(b[0:2], 1)      // htype: Ethernet
	binary.BigEndian.PutUint16(b[2:4], 0x0800) // ptype: IPv4
	b[4] = 6                                   // hlen: MAC address length
	b[5] = 4                                   // plen: IPv4 address length
	binary.BigEndian.PutUint16(b[6:8], p.Operation)
	copy(b[8:14], p.SenderMAC[:])
	copy(b[14:18], p.SenderIP[:])
	copy(b[18:24], p.TargetMAC[:])
	copy(b[24:28], p.TargetIP[:])
	return b
}

// IPv4Header is the decoded fixed 20-byte IPv4 header (no options).
type IPv4Header struct {
	IHL      uint8  // header length in 32-bit words; 5 for no options (20 bytes)
	DSCP     uint8  // differentiated services / ToS
	TotalLen uint16 // total length of the IP datagram (header + payload)
	ID       uint16 // identification field for fragment reassembly
	Flags    uint8  // 3-bit flags field (DF, MF)
	FragOff  uint16 // 13-bit fragment offset
	TTL      uint8
	Protocol uint8
	Checksum uint16
	Src      [4]byte
	Dst      [4]byte
}

// ParseIPv4Header decodes the 20-byte fixed IPv4 header from a raw datagram.
// Options, if present, are not decoded; use HeaderLen to skip past them.
func ParseIPv4Header(b []byte) (IPv4Header, error) {
	if len(b) < 20 {
		return IPv4Header{}, fmt.Errorf("%w: IPv4 header is 20 bytes, got %d", ErrFrameTooShort, len(b))
	}
	var h IPv4Header
	h.IHL = b[0] & 0x0f
	h.DSCP = b[1]
	h.TotalLen = binary.BigEndian.Uint16(b[2:4])
	h.ID = binary.BigEndian.Uint16(b[4:6])
	h.Flags = b[6] >> 5
	h.FragOff = binary.BigEndian.Uint16(b[6:8]) & 0x1fff
	h.TTL = b[8]
	h.Protocol = b[9]
	h.Checksum = binary.BigEndian.Uint16(b[10:12])
	copy(h.Src[:], b[12:16])
	copy(h.Dst[:], b[16:20])
	return h, nil
}

// HeaderLen returns the IPv4 header length in bytes (IHL * 4).
func (h IPv4Header) HeaderLen() int { return int(h.IHL) * 4 }

// Marshal serializes the IPv4 header to 20 bytes.  The checksum field is left
// zero; call InternetChecksum on the result and fill bytes 10:12 before sending.
func (h IPv4Header) Marshal() []byte {
	b := make([]byte, 20)
	b[0] = 0x45 // version=4, IHL=5 (20 bytes, no options)
	b[1] = h.DSCP
	binary.BigEndian.PutUint16(b[2:4], h.TotalLen)
	binary.BigEndian.PutUint16(b[4:6], h.ID)
	binary.BigEndian.PutUint16(b[6:8], uint16(h.Flags)<<13|h.FragOff)
	b[8] = h.TTL
	b[9] = h.Protocol
	// b[10:12] left zero; caller must fill the checksum.
	copy(b[12:16], h.Src[:])
	copy(b[16:20], h.Dst[:])
	return b
}

// ICMPHeader is the 8-byte fixed header for ICMP echo request/reply messages.
type ICMPHeader struct {
	Type     uint8
	Code     uint8
	Checksum uint16
	ID       uint16
	Seq      uint16
}

// ParseICMPHeader decodes an ICMP echo header from an IPv4 payload.
func ParseICMPHeader(b []byte) (ICMPHeader, error) {
	if len(b) < 8 {
		return ICMPHeader{}, fmt.Errorf("%w: ICMP header is 8 bytes, got %d", ErrFrameTooShort, len(b))
	}
	var h ICMPHeader
	h.Type = b[0]
	h.Code = b[1]
	h.Checksum = binary.BigEndian.Uint16(b[2:4])
	h.ID = binary.BigEndian.Uint16(b[4:6])
	h.Seq = binary.BigEndian.Uint16(b[6:8])
	return h, nil
}

// Marshal serializes the ICMP header to 8 bytes.  The checksum is left zero;
// the caller must compute it over the header and payload and fill bytes 2:4.
func (h ICMPHeader) Marshal() []byte {
	b := make([]byte, 8)
	b[0] = h.Type
	b[1] = h.Code
	// b[2:4] left zero; caller fills the checksum.
	binary.BigEndian.PutUint16(b[4:6], h.ID)
	binary.BigEndian.PutUint16(b[6:8], h.Seq)
	return b
}

// IPv4AddrFromNet converts a net.IP (4 or 16 bytes) to a [4]byte array.
func IPv4AddrFromNet(ip net.IP) ([4]byte, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return [4]byte{}, fmt.Errorf("netstack: %v is not an IPv4 address", ip)
	}
	var a [4]byte
	copy(a[:], ip4)
	return a, nil
}
```

### Exercise 2: Internet Checksum and TCP Pseudo-Header

Create `checksum.go`. The same algorithm covers IPv4, ICMP, and TCP; only TCP adds the pseudo-header.

```go
package netstack

import "encoding/binary"

// InternetChecksum computes the RFC 1071 one's-complement checksum over b.
// If b has odd length the final byte is treated as a two-byte word padded with
// a zero byte in the low position.  Pass the result to binary.BigEndian.PutUint16
// to fill the checksum field in a header.
//
// To verify an existing checksum: append the original checksum bytes to the
// data being verified; the result of InternetChecksum must be 0x0000.
func InternetChecksum(b []byte) uint16 {
	var sum uint32
	for len(b) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(b[:2]))
		b = b[2:]
	}
	if len(b) == 1 {
		// Odd byte: treat as high byte of a 16-bit word with low byte = 0.
		sum += uint32(b[0]) << 8
	}
	// Fold 32-bit carry bits back into 16 bits.
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// TCPChecksum computes the TCP checksum using the IPv4 pseudo-header as
// specified in RFC 793 §3.1.  srcIP and dstIP are 4-byte IPv4 addresses.
// tcpSegment must be the TCP header concatenated with the TCP payload; the
// checksum field in the header must be zero before calling this function.
func TCPChecksum(srcIP, dstIP [4]byte, tcpSegment []byte) uint16 {
	// IPv4 pseudo-header: src(4) + dst(4) + zero(1) + proto(1) + tcp-length(2).
	pseudo := make([]byte, 12+len(tcpSegment))
	copy(pseudo[0:4], srcIP[:])
	copy(pseudo[4:8], dstIP[:])
	pseudo[8] = 0 // reserved
	pseudo[9] = ProtoTCP
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(tcpSegment)))
	copy(pseudo[12:], tcpSegment)
	return InternetChecksum(pseudo)
}

// ICMPChecksumFill fills the checksum field (bytes 2:4) of an ICMP message
// that includes its header and payload concatenated in msg.  The checksum
// field must be zero on entry.
func ICMPChecksumFill(msg []byte) {
	if len(msg) < 4 {
		return
	}
	cs := InternetChecksum(msg)
	binary.BigEndian.PutUint16(msg[2:4], cs)
}
```

### Exercise 3: TCP Header and State Machine

Create `tcp.go`. The `Conn` type owns the RFC 793 sequence variables and advances the state machine in response to incoming segments. All methods are safe for concurrent use via `sync.Mutex`.

```go
package netstack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sync"
)

// TCPState enumerates the states of the TCP finite state machine (RFC 793 §3.2).
type TCPState int

const (
	StateClosed      TCPState = iota
	StateListen               // passive open; waiting for incoming SYN
	StateSynSent              // active open; SYN sent, waiting for SYN-ACK
	StateSynReceived          // SYN received; SYN-ACK sent, waiting for ACK
	StateEstablished          // connection open; normal data transfer
	StateFinWait1             // FIN sent; waiting for FIN or ACK
	StateFinWait2             // received ACK of FIN; waiting for remote FIN
	StateCloseWait            // received FIN; waiting for application close
	StateClosing              // both sides sent FIN; waiting for ACK
	StateLastAck              // passive close; waiting for ACK of FIN
	StateTimeWait             // waiting for 2*MSL before releasing the TCB
)

// String returns the RFC 793 state name.
func (s TCPState) String() string {
	names := [...]string{
		"CLOSED", "LISTEN", "SYN_SENT", "SYN_RECEIVED",
		"ESTABLISHED", "FIN_WAIT_1", "FIN_WAIT_2",
		"CLOSE_WAIT", "CLOSING", "LAST_ACK", "TIME_WAIT",
	}
	if int(s) < len(names) {
		return names[s]
	}
	return fmt.Sprintf("TCPState(%d)", int(s))
}

// TCP flag bit positions in the flags byte of the TCP header.
const (
	FlagFIN uint8 = 1 << 0
	FlagSYN uint8 = 1 << 1
	FlagRST uint8 = 1 << 2
	FlagPSH uint8 = 1 << 3
	FlagACK uint8 = 1 << 4
	FlagURG uint8 = 1 << 5
)

// Sentinel errors for TCP operations.
var (
	ErrInvalidSegment   = errors.New("netstack: invalid TCP segment")
	ErrConnectionReset  = errors.New("netstack: connection reset by peer")
	ErrConnectionClosed = errors.New("netstack: connection already closed")
	ErrWrongState       = errors.New("netstack: operation not valid in current state")
)

// TCPHeader is the decoded fixed 20-byte TCP header (no options).
type TCPHeader struct {
	SrcPort    uint16
	DstPort    uint16
	SeqNum     uint32
	AckNum     uint32
	DataOffset uint8  // header length in 32-bit words; 5 = 20 bytes (no options)
	Flags      uint8  // bitmask of Flag* constants
	WindowSize uint16 // receive window advertised by the sender
	Checksum   uint16
	Urgent     uint16
}

// ParseTCPHeader decodes the fixed 20-byte TCP header from a raw segment.
func ParseTCPHeader(b []byte) (TCPHeader, error) {
	if len(b) < 20 {
		return TCPHeader{}, fmt.Errorf("%w: TCP header needs 20 bytes, got %d", ErrInvalidSegment, len(b))
	}
	var h TCPHeader
	h.SrcPort = binary.BigEndian.Uint16(b[0:2])
	h.DstPort = binary.BigEndian.Uint16(b[2:4])
	h.SeqNum = binary.BigEndian.Uint32(b[4:8])
	h.AckNum = binary.BigEndian.Uint32(b[8:12])
	h.DataOffset = b[12] >> 4
	h.Flags = b[13]
	h.WindowSize = binary.BigEndian.Uint16(b[14:16])
	h.Checksum = binary.BigEndian.Uint16(b[16:18])
	h.Urgent = binary.BigEndian.Uint16(b[18:20])
	return h, nil
}

// Marshal serializes the TCP header to 20 bytes.  The checksum field is left
// zero; compute TCPChecksum over the result and the payload, then fill bytes
// 16:18 before sending.
func (h TCPHeader) Marshal() []byte {
	b := make([]byte, 20)
	binary.BigEndian.PutUint16(b[0:2], h.SrcPort)
	binary.BigEndian.PutUint16(b[2:4], h.DstPort)
	binary.BigEndian.PutUint32(b[4:8], h.SeqNum)
	binary.BigEndian.PutUint32(b[8:12], h.AckNum)
	b[12] = h.DataOffset << 4
	b[13] = h.Flags
	binary.BigEndian.PutUint16(b[14:16], h.WindowSize)
	// b[16:18] left zero; caller fills the checksum.
	binary.BigEndian.PutUint16(b[18:20], h.Urgent)
	return b
}

// HasFlag reports whether the given flag bit is set.
func (h TCPHeader) HasFlag(flag uint8) bool { return h.Flags&flag != 0 }

// Conn is a single userspace TCP connection.  It tracks the RFC 793 sequence
// variables and advances the state machine in response to incoming segments.
// All exported methods are safe for concurrent use.
type Conn struct {
	mu sync.Mutex

	state TCPState

	// Send-side sequence variables (RFC 793 §3.2).
	iss    uint32 // initial send sequence number
	sndUNA uint32 // oldest unacknowledged sequence number (SND.UNA)
	sndNXT uint32 // next sequence number to send (SND.NXT)
	sndWND uint32 // send window: peer's advertised receive window (SND.WND)

	// Receive-side sequence variables.
	irs    uint32 // initial receive sequence number
	rcvNXT uint32 // next expected receive sequence number (RCV.NXT)
	rcvWND uint32 // receive window we advertise (RCV.WND)
}

// NewConn creates a TCP connection in the LISTEN state, ready for a passive open.
func NewConn() *Conn {
	return &Conn{
		state:  StateListen,
		rcvWND: 65535,
	}
}

// State returns the current TCP state.
func (c *Conn) State() TCPState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// RecvSYN processes an incoming SYN segment and advances the state machine from
// LISTEN to SYN_RECEIVED.  It returns the SYN-ACK segment to transmit.
// The returned header has Checksum=0; the caller must fill it before sending.
func (c *Conn) RecvSYN(seg TCPHeader) (TCPHeader, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state != StateListen {
		return TCPHeader{}, fmt.Errorf("%w: RecvSYN requires LISTEN, got %s", ErrWrongState, c.state)
	}
	if !seg.HasFlag(FlagSYN) {
		return TCPHeader{}, fmt.Errorf("%w: expected SYN flag, got %#02x", ErrInvalidSegment, seg.Flags)
	}

	c.irs = seg.SeqNum
	c.rcvNXT = seg.SeqNum + 1 // SYN consumes one sequence number
	c.iss = rand.Uint32()
	c.sndNXT = c.iss + 1 // SYN-ACK consumes one sequence number
	c.sndUNA = c.iss
	c.state = StateSynReceived

	return TCPHeader{
		SrcPort:    seg.DstPort,
		DstPort:    seg.SrcPort,
		SeqNum:     c.iss,
		AckNum:     c.rcvNXT,
		DataOffset: 5,
		Flags:      FlagSYN | FlagACK,
		WindowSize: uint16(c.rcvWND),
	}, nil
}

// RecvACK processes the final ACK of the three-way handshake and advances the
// state machine from SYN_RECEIVED to ESTABLISHED.
func (c *Conn) RecvACK(seg TCPHeader) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state != StateSynReceived {
		return fmt.Errorf("%w: RecvACK requires SYN_RECEIVED, got %s", ErrWrongState, c.state)
	}
	if !seg.HasFlag(FlagACK) {
		return fmt.Errorf("%w: expected ACK flag, got %#02x", ErrInvalidSegment, seg.Flags)
	}
	if seg.AckNum != c.sndNXT {
		return fmt.Errorf("%w: ack %d does not match sndNXT %d", ErrInvalidSegment, seg.AckNum, c.sndNXT)
	}

	c.sndUNA = seg.AckNum
	c.sndWND = uint32(seg.WindowSize)
	c.state = StateEstablished
	return nil
}

// RecvFIN processes an incoming FIN segment.  If the connection is ESTABLISHED
// it advances to CLOSE_WAIT and returns the ACK to send.  If it is FIN_WAIT_1
// or FIN_WAIT_2 it advances to TIME_WAIT.
func (c *Conn) RecvFIN(seg TCPHeader) (TCPHeader, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !seg.HasFlag(FlagFIN) {
		return TCPHeader{}, fmt.Errorf("%w: expected FIN flag, got %#02x", ErrInvalidSegment, seg.Flags)
	}

	c.rcvNXT = seg.SeqNum + 1 // FIN consumes one sequence number

	var nextState TCPState
	switch c.state {
	case StateEstablished:
		nextState = StateCloseWait
	case StateFinWait1, StateFinWait2:
		nextState = StateTimeWait
	default:
		return TCPHeader{}, fmt.Errorf("%w: RecvFIN in %s", ErrWrongState, c.state)
	}
	c.state = nextState

	return TCPHeader{
		SrcPort:    seg.DstPort,
		DstPort:    seg.SrcPort,
		SeqNum:     c.sndNXT,
		AckNum:     c.rcvNXT,
		DataOffset: 5,
		Flags:      FlagACK,
		WindowSize: uint16(c.rcvWND),
	}, nil
}

// CanSend returns how many bytes the sender may transmit without violating the
// send window.  It returns 0 if the window is fully consumed or zero-sized.
func (c *Conn) CanSend() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()

	inFlight := c.sndNXT - c.sndUNA
	if c.sndWND == 0 || inFlight >= c.sndWND {
		return 0
	}
	return c.sndWND - inFlight
}

// AdvanceSND updates SND.NXT after n bytes have been transmitted.  It does not
// move SND.UNA; that is the job of RecvDataACK.
func (c *Conn) AdvanceSND(n uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sndNXT += n
}

// RecvDataACK advances SND.UNA to ackNum and updates the send window.
// It returns an error if ackNum is outside the valid range [sndUNA, sndNXT].
func (c *Conn) RecvDataACK(ackNum uint32, window uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Use modular arithmetic to handle sequence number wraparound.
	// ackNum must be in (sndUNA, sndNXT] to be valid.
	if ackNum == c.sndUNA {
		// Duplicate ACK; update window only.
		c.sndWND = uint32(window)
		return nil
	}
	ahead := ackNum - c.sndUNA // modular distance
	window32 := c.sndNXT - c.sndUNA
	if ahead > window32 {
		return fmt.Errorf("%w: ack %d outside [%d, %d]", ErrInvalidSegment, ackNum, c.sndUNA, c.sndNXT)
	}
	c.sndUNA = ackNum
	c.sndWND = uint32(window)
	return nil
}
```

### Exercise 4: TAP Device and Dispatch Loop (Linux only)

Create `tap_linux.go`. This file uses `golang.org/x/sys/unix` for the `ioctl` call and only compiles on Linux.

```go
//go:build linux

package netstack

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	tunSetIFF = 0x400454ca // TUNSETIFF ioctl number
	iffTAP    = 0x0002
	iffNOPI   = 0x1000
)

// OpenTAP opens (or creates) a TAP device with the given name and returns the
// os.File wrapping its file descriptor.  The device delivers raw Ethernet frames
// without the four-byte packet-info prefix (IFF_NO_PI).
func OpenTAP(name string) (*os.File, error) {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}

	// struct ifreq: 16-byte name + 2-byte flags, padded to 40 bytes.
	var ifr [40]byte
	copy(ifr[:16], name)
	binary.LittleEndian.PutUint16(ifr[16:18], iffTAP|iffNOPI)

	if _, _, errno := unix.Syscall(
		unix.SYS_IOCTL, f.Fd(), tunSetIFF, uintptr(unsafe.Pointer(&ifr[0])),
	); errno != 0 {
		f.Close()
		return nil, fmt.Errorf("TUNSETIFF: %w", errno)
	}
	return f, nil
}

// Stack is the running protocol stack.  Create one with New, then call Run in
// a goroutine to process incoming frames.
type Stack struct {
	f        *os.File
	myMAC    HardwareAddr
	myIP     [4]byte
	arpCache map[[4]byte]HardwareAddr
	conns    map[uint32]*Conn // keyed by remote IP as uint32
}

// NewStack creates a Stack that will claim the given MAC and IP address.
func NewStack(f *os.File, mac HardwareAddr, ip net.IP) (*Stack, error) {
	addr, err := IPv4AddrFromNet(ip)
	if err != nil {
		return nil, err
	}
	return &Stack{
		f:        f,
		myMAC:    mac,
		myIP:     addr,
		arpCache: make(map[[4]byte]HardwareAddr),
		conns:    make(map[uint32]*Conn),
	}, nil
}

// Run reads frames from the TAP device and dispatches them until f is closed.
func (s *Stack) Run() error {
	buf := make([]byte, 1518) // max Ethernet frame (no VLAN)
	for {
		n, err := s.f.Read(buf)
		if err != nil {
			return err
		}
		s.handleFrame(buf[:n])
	}
}

func (s *Stack) handleFrame(raw []byte) {
	frame, err := ParseEtherFrame(raw)
	if err != nil {
		return
	}
	switch frame.EtherType {
	case EtherTypeARP:
		s.handleARP(frame)
	case EtherTypeIPv4:
		s.handleIPv4(frame)
	}
}

func (s *Stack) handleARP(frame EtherFrame) {
	pkt, err := ParseARPPacket(frame.Payload)
	if err != nil {
		return
	}
	// Cache the sender's address regardless of the operation.
	s.arpCache[pkt.SenderIP] = pkt.SenderMAC

	if pkt.Operation != ARPRequest || pkt.TargetIP != s.myIP {
		return
	}
	reply := ARPPacket{
		Operation: ARPReply,
		SenderMAC: s.myMAC,
		SenderIP:  s.myIP,
		TargetMAC: pkt.SenderMAC,
		TargetIP:  pkt.SenderIP,
	}
	out := EtherFrame{
		Dst:       pkt.SenderMAC,
		Src:       s.myMAC,
		EtherType: EtherTypeARP,
		Payload:   reply.Marshal(),
	}
	s.f.Write(out.Marshal()) //nolint:errcheck
}

func (s *Stack) handleIPv4(frame EtherFrame) {
	hdr, err := ParseIPv4Header(frame.Payload)
	if err != nil || hdr.Dst != s.myIP {
		return
	}
	// Verify header checksum.
	headerBytes := frame.Payload[:hdr.HeaderLen()]
	if InternetChecksum(headerBytes) != 0 {
		return
	}
	payload := frame.Payload[hdr.HeaderLen():]
	switch hdr.Protocol {
	case ProtoICMP:
		s.handleICMP(frame.Src, hdr, payload)
	case ProtoTCP:
		s.handleTCP(hdr, payload)
	}
}

func (s *Stack) handleICMP(srcMAC HardwareAddr, iph IPv4Header, payload []byte) {
	req, err := ParseICMPHeader(payload)
	if err != nil || req.Type != ICMPTypeEchoRequest {
		return
	}
	// Build reply header + copy the echo data.
	replyHdr := ICMPHeader{
		Type: ICMPTypeEchoReply,
		ID:   req.ID,
		Seq:  req.Seq,
	}
	icmpMsg := append(replyHdr.Marshal(), payload[8:]...)
	ICMPChecksumFill(icmpMsg)

	ipHdr := IPv4Header{
		IHL: 5, TotalLen: uint16(20 + len(icmpMsg)),
		TTL: 64, Protocol: ProtoICMP,
		Src: s.myIP, Dst: iph.Src,
	}
	ipRaw := ipHdr.Marshal()
	binary.BigEndian.PutUint16(ipRaw[10:12], InternetChecksum(ipRaw))

	out := EtherFrame{
		Dst: srcMAC, Src: s.myMAC,
		EtherType: EtherTypeIPv4,
		Payload:   append(ipRaw, icmpMsg...),
	}
	s.f.Write(out.Marshal()) //nolint:errcheck
}

func (s *Stack) handleTCP(iph IPv4Header, payload []byte) {
	seg, err := ParseTCPHeader(payload)
	if err != nil {
		return
	}
	// Verify TCP checksum.
	if TCPChecksum(iph.Src, iph.Dst, payload) != 0 {
		return
	}
	key := binary.BigEndian.Uint32(iph.Src[:])
	conn, ok := s.conns[key]
	if !ok {
		conn = NewConn()
		s.conns[key] = conn
	}
	if seg.HasFlag(FlagSYN) && !seg.HasFlag(FlagACK) {
		synAck, err := conn.RecvSYN(seg)
		if err != nil {
			return
		}
		s.sendTCP(iph.Src, synAck, nil)
	} else if seg.HasFlag(FlagACK) && !seg.HasFlag(FlagSYN) {
		if conn.State() == StateSynReceived {
			conn.RecvACK(seg) //nolint:errcheck
		}
	}
}

func (s *Stack) sendTCP(dst [4]byte, hdr TCPHeader, data []byte) {
	segment := append(hdr.Marshal(), data...)
	binary.BigEndian.PutUint16(segment[16:18], TCPChecksum(s.myIP, dst, segment))

	ipHdr := IPv4Header{
		IHL: 5, TotalLen: uint16(20 + len(segment)),
		TTL: 64, Protocol: ProtoTCP,
		Src: s.myIP, Dst: dst,
	}
	ipRaw := ipHdr.Marshal()
	binary.BigEndian.PutUint16(ipRaw[10:12], InternetChecksum(ipRaw))

	dstMAC, ok := s.arpCache[dst]
	if !ok {
		return // drop; in a real stack you would queue and ARP-resolve
	}
	out := EtherFrame{
		Dst: dstMAC, Src: s.myMAC,
		EtherType: EtherTypeIPv4,
		Payload:   append(ipRaw, segment...),
	}
	s.f.Write(out.Marshal()) //nolint:errcheck
}
```

To run the full stack on Linux (requires `sudo` for TAP device creation):

```bash
go get golang.org/x/sys
go build ./...
sudo ./netstack         # or go run ./cmd/demo on Linux with tap privileges
sudo ip addr add 10.0.0.1/24 dev tap0
sudo ip link set tap0 up
ping 10.0.0.2           # should receive replies from your stack
curl http://10.0.0.2/  # requires adding an HTTP handler on top of TCP
```

### Exercise 5: Tests

Create `netstack_test.go`. The tests cover all extractable (pure-Go) layers and run without a TAP device.

```go
package netstack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

// ---------------------------------------------------------------------------
// Ethernet
// ---------------------------------------------------------------------------

func TestParseEtherFrameRejectsShortInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"13 bytes", make([]byte, 13)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseEtherFrame(tc.b)
			if !errors.Is(err, ErrFrameTooShort) {
				t.Fatalf("ParseEtherFrame(%d bytes): err = %v, want ErrFrameTooShort", len(tc.b), err)
			}
		})
	}
}

func TestParseEtherFrameRoundTrip(t *testing.T) {
	t.Parallel()
	original := EtherFrame{
		Dst:       HardwareAddr{0x01, 0x02, 0x03, 0x04, 0x05, 0x06},
		Src:       HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
		EtherType: EtherTypeIPv4,
		Payload:   []byte{0xde, 0xad, 0xbe, 0xef},
	}
	parsed, err := ParseEtherFrame(original.Marshal())
	if err != nil {
		t.Fatalf("ParseEtherFrame: %v", err)
	}
	if parsed.Dst != original.Dst {
		t.Fatalf("Dst = %s, want %s", parsed.Dst, original.Dst)
	}
	if parsed.Src != original.Src {
		t.Fatalf("Src = %s, want %s", parsed.Src, original.Src)
	}
	if parsed.EtherType != EtherTypeIPv4 {
		t.Fatalf("EtherType = %#04x, want %#04x", parsed.EtherType, uint16(EtherTypeIPv4))
	}
	if string(parsed.Payload) != string(original.Payload) {
		t.Fatalf("Payload mismatch: %v vs %v", parsed.Payload, original.Payload)
	}
}

// ---------------------------------------------------------------------------
// Checksum
// ---------------------------------------------------------------------------

func TestInternetChecksumKnownVector(t *testing.T) {
	t.Parallel()
	// Manually verified: sum of 0x0001+0xf203+0xf4f5+0xf6f7 = 0x1ddf2
	// after folding = 0xddf2, one's complement = 0x220d.
	data := []byte{0x00, 0x01, 0xf2, 0x03, 0xf4, 0xf5, 0xf6, 0xf7}
	if got := InternetChecksum(data); got != 0x220d {
		t.Fatalf("InternetChecksum = %#04x, want 0x220d", got)
	}
}

func TestInternetChecksumVerify(t *testing.T) {
	t.Parallel()
	// Build an IPv4 header, fill the checksum field, then verify that re-running
	// the checksum over the header (including the filled checksum) yields 0x0000.
	raw := []byte{
		0x45, 0x00, 0x00, 0x3c, // ver/IHL, DSCP, total length
		0x1c, 0x46, 0x40, 0x00, // ID, flags, fragment offset
		0x40, 0x06, 0x00, 0x00, // TTL, protocol, checksum (zeroed)
		0xac, 0x10, 0x0a, 0x63, // src IP: 172.16.10.99
		0xac, 0x10, 0x0a, 0x0c, // dst IP: 172.16.10.12
	}
	cs := InternetChecksum(raw)
	binary.BigEndian.PutUint16(raw[10:12], cs)
	if got := InternetChecksum(raw); got != 0 {
		t.Fatalf("verify: InternetChecksum with filled checksum = %#04x, want 0x0000", got)
	}
}

func TestInternetChecksumOddLength(t *testing.T) {
	t.Parallel()
	// Odd-length input: the final byte must be treated as the high byte of a
	// 16-bit word (low byte = 0).  Checksum of {0x01} = ~0x0100 = 0xfeff.
	if got := InternetChecksum([]byte{0x01}); got != 0xfeff {
		t.Fatalf("InternetChecksum([0x01]) = %#04x, want 0xfeff", got)
	}
}

func TestTCPChecksumSymmetry(t *testing.T) {
	t.Parallel()
	src := [4]byte{10, 0, 0, 1}
	dst := [4]byte{10, 0, 0, 2}
	seg := make([]byte, 20) // minimal TCP header, all zeros
	seg[12] = 5 << 4        // DataOffset = 5
	cs := TCPChecksum(src, dst, seg)
	binary.BigEndian.PutUint16(seg[16:18], cs)
	if got := TCPChecksum(src, dst, seg); got != 0 {
		t.Fatalf("verify: TCPChecksum with filled checksum = %#04x, want 0x0000", got)
	}
}

// ---------------------------------------------------------------------------
// ARP
// ---------------------------------------------------------------------------

func TestARPPacketRoundTrip(t *testing.T) {
	t.Parallel()
	original := ARPPacket{
		Operation: ARPReply,
		SenderMAC: HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
		SenderIP:  [4]byte{10, 0, 0, 2},
		TargetMAC: HardwareAddr{0x01, 0x02, 0x03, 0x04, 0x05, 0x06},
		TargetIP:  [4]byte{10, 0, 0, 1},
	}
	parsed, err := ParseARPPacket(original.Marshal())
	if err != nil {
		t.Fatalf("ParseARPPacket: %v", err)
	}
	if parsed.Operation != ARPReply {
		t.Fatalf("Operation = %d, want ARPReply (%d)", parsed.Operation, ARPReply)
	}
	if parsed.SenderMAC != original.SenderMAC {
		t.Fatalf("SenderMAC = %s, want %s", parsed.SenderMAC, original.SenderMAC)
	}
	if parsed.SenderIP != original.SenderIP {
		t.Fatalf("SenderIP = %v, want %v", parsed.SenderIP, original.SenderIP)
	}
	if parsed.TargetIP != original.TargetIP {
		t.Fatalf("TargetIP = %v, want %v", parsed.TargetIP, original.TargetIP)
	}
}

func TestARPPacketRejectsShort(t *testing.T) {
	t.Parallel()
	_, err := ParseARPPacket(make([]byte, 27))
	if !errors.Is(err, ErrFrameTooShort) {
		t.Fatalf("err = %v, want ErrFrameTooShort", err)
	}
}

// ---------------------------------------------------------------------------
// IPv4
// ---------------------------------------------------------------------------

func TestIPv4HeaderRoundTrip(t *testing.T) {
	t.Parallel()
	original := IPv4Header{
		IHL:      5,
		TotalLen: 60,
		ID:       0x1c46,
		TTL:      64,
		Protocol: ProtoTCP,
		Src:      [4]byte{192, 168, 1, 1},
		Dst:      [4]byte{192, 168, 1, 2},
	}
	raw := original.Marshal()
	parsed, err := ParseIPv4Header(raw)
	if err != nil {
		t.Fatalf("ParseIPv4Header: %v", err)
	}
	if parsed.Protocol != ProtoTCP {
		t.Fatalf("Protocol = %d, want ProtoTCP (%d)", parsed.Protocol, ProtoTCP)
	}
	if parsed.Src != original.Src || parsed.Dst != original.Dst {
		t.Fatalf("addresses: src=%v dst=%v", parsed.Src, parsed.Dst)
	}
	if parsed.HeaderLen() != 20 {
		t.Fatalf("HeaderLen = %d, want 20", parsed.HeaderLen())
	}
}

// ---------------------------------------------------------------------------
// TCP header
// ---------------------------------------------------------------------------

func TestTCPHeaderRoundTrip(t *testing.T) {
	t.Parallel()
	original := TCPHeader{
		SrcPort:    54321,
		DstPort:    80,
		SeqNum:     0xdeadbeef,
		AckNum:     0xcafebabe,
		DataOffset: 5,
		Flags:      FlagSYN | FlagACK,
		WindowSize: 65535,
	}
	parsed, err := ParseTCPHeader(original.Marshal())
	if err != nil {
		t.Fatalf("ParseTCPHeader: %v", err)
	}
	if parsed.SrcPort != original.SrcPort || parsed.DstPort != original.DstPort {
		t.Fatalf("ports: %d/%d, want %d/%d", parsed.SrcPort, parsed.DstPort, original.SrcPort, original.DstPort)
	}
	if parsed.SeqNum != original.SeqNum || parsed.AckNum != original.AckNum {
		t.Fatalf("seq/ack: %d/%d, want %d/%d", parsed.SeqNum, parsed.AckNum, original.SeqNum, original.AckNum)
	}
	if !parsed.HasFlag(FlagSYN) || !parsed.HasFlag(FlagACK) {
		t.Fatalf("Flags = %#02x, want SYN|ACK", parsed.Flags)
	}
	if parsed.HasFlag(FlagFIN) {
		t.Fatalf("FIN should not be set")
	}
}

func TestTCPHeaderRejectsShort(t *testing.T) {
	t.Parallel()
	_, err := ParseTCPHeader(make([]byte, 19))
	if !errors.Is(err, ErrInvalidSegment) {
		t.Fatalf("err = %v, want ErrInvalidSegment", err)
	}
}

// ---------------------------------------------------------------------------
// TCP state machine
// ---------------------------------------------------------------------------

func TestTCPThreeWayHandshake(t *testing.T) {
	t.Parallel()

	conn := NewConn()
	if conn.State() != StateListen {
		t.Fatalf("initial state = %s, want LISTEN", conn.State())
	}

	// Step 1 (client -> server): SYN.
	synSeg := TCPHeader{
		SrcPort: 54321, DstPort: 80,
		SeqNum: 1000, Flags: FlagSYN, DataOffset: 5, WindowSize: 65535,
	}
	synAck, err := conn.RecvSYN(synSeg)
	if err != nil {
		t.Fatalf("RecvSYN: %v", err)
	}
	if conn.State() != StateSynReceived {
		t.Fatalf("after RecvSYN: state = %s, want SYN_RECEIVED", conn.State())
	}
	if !synAck.HasFlag(FlagSYN) || !synAck.HasFlag(FlagACK) {
		t.Fatalf("SYN-ACK flags = %#02x, want SYN+ACK", synAck.Flags)
	}
	if synAck.AckNum != 1001 {
		t.Fatalf("SYN-ACK AckNum = %d, want 1001 (client ISN + 1)", synAck.AckNum)
	}

	// Step 2 (client -> server): ACK of the SYN-ACK.
	ackSeg := TCPHeader{
		SrcPort: 54321, DstPort: 80,
		SeqNum: 1001, AckNum: synAck.SeqNum + 1,
		Flags: FlagACK, DataOffset: 5, WindowSize: 65535,
	}
	if err := conn.RecvACK(ackSeg); err != nil {
		t.Fatalf("RecvACK: %v", err)
	}
	if conn.State() != StateEstablished {
		t.Fatalf("after RecvACK: state = %s, want ESTABLISHED", conn.State())
	}
}

func TestTCPRecvSYNRejectsNonListenState(t *testing.T) {
	t.Parallel()
	conn := NewConn()
	synSeg := TCPHeader{Flags: FlagSYN, DataOffset: 5, WindowSize: 65535}
	if _, err := conn.RecvSYN(synSeg); err != nil {
		t.Fatalf("first RecvSYN: %v", err)
	}
	// A second SYN while in SYN_RECEIVED must return ErrWrongState.
	_, err := conn.RecvSYN(synSeg)
	if !errors.Is(err, ErrWrongState) {
		t.Fatalf("second RecvSYN: err = %v, want ErrWrongState", err)
	}
}

func TestTCPRecvACKRejectsBadAckNum(t *testing.T) {
	t.Parallel()
	conn := NewConn()
	synSeg := TCPHeader{SeqNum: 500, Flags: FlagSYN, DataOffset: 5, WindowSize: 8192}
	synAck, err := conn.RecvSYN(synSeg)
	if err != nil {
		t.Fatal(err)
	}
	badACK := TCPHeader{
		SeqNum: 501, AckNum: synAck.SeqNum + 99, // wrong: should be +1
		Flags: FlagACK, DataOffset: 5, WindowSize: 8192,
	}
	if err := conn.RecvACK(badACK); !errors.Is(err, ErrInvalidSegment) {
		t.Fatalf("RecvACK with wrong ackNum: err = %v, want ErrInvalidSegment", err)
	}
}

func TestTCPCanSendRespectsWindow(t *testing.T) {
	t.Parallel()
	const peerWindow = 4096

	conn := doHandshake(t, 1000, peerWindow)
	if got := conn.CanSend(); got != peerWindow {
		t.Fatalf("CanSend after handshake = %d, want %d", got, peerWindow)
	}

	conn.AdvanceSND(1000)
	if got := conn.CanSend(); got != peerWindow-1000 {
		t.Fatalf("CanSend after 1000 sent = %d, want %d", got, peerWindow-1000)
	}

	conn.mu.Lock()
	ackNum := conn.sndUNA + 1000
	conn.mu.Unlock()
	if err := conn.RecvDataACK(ackNum, peerWindow); err != nil {
		t.Fatal(err)
	}
	if got := conn.CanSend(); got != peerWindow {
		t.Fatalf("CanSend after ACK = %d, want %d", got, peerWindow)
	}
}

func TestTCPRecvFINFromEstablished(t *testing.T) {
	t.Parallel()
	conn := doHandshake(t, 1000, 65535)

	finSeg := TCPHeader{
		SrcPort: 54321, DstPort: 80,
		SeqNum: 1001, Flags: FlagFIN | FlagACK, DataOffset: 5,
	}
	ack, err := conn.RecvFIN(finSeg)
	if err != nil {
		t.Fatalf("RecvFIN: %v", err)
	}
	if conn.State() != StateCloseWait {
		t.Fatalf("state = %s, want CLOSE_WAIT", conn.State())
	}
	if !ack.HasFlag(FlagACK) {
		t.Fatalf("RecvFIN ACK flags = %#02x, want ACK", ack.Flags)
	}
}

// doHandshake runs a complete three-way handshake and returns the established Conn.
func doHandshake(t *testing.T, clientISN uint32, peerWindow uint16) *Conn {
	t.Helper()
	conn := NewConn()
	synSeg := TCPHeader{
		SrcPort: 54321, DstPort: 80,
		SeqNum: clientISN, Flags: FlagSYN, DataOffset: 5, WindowSize: peerWindow,
	}
	synAck, err := conn.RecvSYN(synSeg)
	if err != nil {
		t.Fatalf("doHandshake RecvSYN: %v", err)
	}
	ackSeg := TCPHeader{
		SrcPort: 54321, DstPort: 80,
		SeqNum: clientISN + 1, AckNum: synAck.SeqNum + 1,
		Flags: FlagACK, DataOffset: 5, WindowSize: peerWindow,
	}
	if err := conn.RecvACK(ackSeg); err != nil {
		t.Fatalf("doHandshake RecvACK: %v", err)
	}
	return conn
}

// Your turn: add TestTCPRecvDataACKRejectsOutOfRange that calls RecvDataACK
// with an ackNum beyond sndNXT and asserts errors.Is(err, ErrInvalidSegment).

// ---------------------------------------------------------------------------
// Example (auto-verified by go test)
// ---------------------------------------------------------------------------

func ExampleHardwareAddr_String() {
	mac := HardwareAddr{0x00, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e}
	fmt.Println(mac)
	// Output: 00:1a:2b:3c:4d:5e
}
```

### Exercise 6: Demo Binary

Create `cmd/demo/main.go`. It exercises the exported API without a TAP device and runs on any OS.

```go
// Command demo exercises the netstack package without opening a real TAP
// device.  It demonstrates ARP reply construction, IPv4 header parsing,
// ICMP echo reply building, TCP header round-trip, and the three-way
// handshake state machine.  Run with:
//
//	go run ./cmd/demo
package main

import (
	"encoding/binary"
	"fmt"
	"net"

	"example.com/netstack"
)

func main() {
	demoEthernet()
	demoARP()
	demoIPv4()
	demoICMP()
	demoTCPHandshake()
}

func demoEthernet() {
	fmt.Println("=== Ethernet ===")
	f := netstack.EtherFrame{
		Dst:       netstack.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		Src:       netstack.HardwareAddr{0x52, 0x54, 0x00, 0x12, 0x34, 0x56},
		EtherType: netstack.EtherTypeARP,
		Payload:   make([]byte, 28),
	}
	raw := f.Marshal()
	parsed, err := netstack.ParseEtherFrame(raw)
	if err != nil {
		panic(err)
	}
	fmt.Printf("  src  %s\n", parsed.Src)
	fmt.Printf("  dst  %s\n", parsed.Dst)
	fmt.Printf("  type %#04x (ARP)\n", parsed.EtherType)
}

func demoARP() {
	fmt.Println("=== ARP ===")
	stackMAC := netstack.HardwareAddr{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}
	hostMAC := netstack.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0xcc}
	reply := netstack.ARPPacket{
		Operation: netstack.ARPReply,
		SenderMAC: stackMAC,
		SenderIP:  [4]byte{10, 0, 0, 2},
		TargetMAC: hostMAC,
		TargetIP:  [4]byte{10, 0, 0, 1},
	}
	raw := reply.Marshal()
	parsed, err := netstack.ParseARPPacket(raw)
	if err != nil {
		panic(err)
	}
	fmt.Printf("  sender %s @ %v\n", parsed.SenderMAC, net.IP(parsed.SenderIP[:]))
	fmt.Printf("  target %s @ %v\n", parsed.TargetMAC, net.IP(parsed.TargetIP[:]))
}

func demoIPv4() {
	fmt.Println("=== IPv4 ===")
	h := netstack.IPv4Header{
		IHL:      5,
		TotalLen: 40,
		TTL:      64,
		Protocol: netstack.ProtoTCP,
		Src:      [4]byte{10, 0, 0, 2},
		Dst:      [4]byte{10, 0, 0, 1},
	}
	raw := h.Marshal()
	cs := netstack.InternetChecksum(raw)
	binary.BigEndian.PutUint16(raw[10:12], cs)

	parsed, err := netstack.ParseIPv4Header(raw)
	if err != nil {
		panic(err)
	}
	fmt.Printf("  src      %v\n", net.IP(parsed.Src[:]))
	fmt.Printf("  dst      %v\n", net.IP(parsed.Dst[:]))
	fmt.Printf("  protocol %d (TCP)\n", parsed.Protocol)
	fmt.Printf("  checksum %#04x\n", parsed.Checksum)

	if netstack.InternetChecksum(raw) != 0 {
		panic("header checksum verification failed")
	}
	fmt.Println("  checksum OK")
}

func demoICMP() {
	fmt.Println("=== ICMP echo reply ===")
	request := netstack.ICMPHeader{
		Type: netstack.ICMPTypeEchoRequest,
		ID:   0x0042,
		Seq:  1,
	}
	reply := netstack.ICMPHeader{
		Type: netstack.ICMPTypeEchoReply,
		ID:   request.ID,
		Seq:  request.Seq,
	}
	data := reply.Marshal()
	netstack.ICMPChecksumFill(data)

	parsed, err := netstack.ParseICMPHeader(data)
	if err != nil {
		panic(err)
	}
	fmt.Printf("  type %d (echo reply)\n", parsed.Type)
	fmt.Printf("  id   %#04x\n", parsed.ID)
	fmt.Printf("  seq  %d\n", parsed.Seq)
}

func demoTCPHandshake() {
	fmt.Println("=== TCP three-way handshake ===")
	conn := netstack.NewConn()
	fmt.Printf("  initial state: %s\n", conn.State())

	synSeg := netstack.TCPHeader{
		SrcPort:    54321,
		DstPort:    80,
		SeqNum:     1000,
		Flags:      netstack.FlagSYN,
		DataOffset: 5,
		WindowSize: 65535,
	}
	synAck, err := conn.RecvSYN(synSeg)
	if err != nil {
		panic(err)
	}
	fmt.Printf("  after SYN:     %s  (SYN-ACK ackNum=%d)\n", conn.State(), synAck.AckNum)

	ackSeg := netstack.TCPHeader{
		SrcPort:    54321,
		DstPort:    80,
		SeqNum:     1001,
		AckNum:     synAck.SeqNum + 1,
		Flags:      netstack.FlagACK,
		DataOffset: 5,
		WindowSize: 65535,
	}
	if err := conn.RecvACK(ackSeg); err != nil {
		panic(err)
	}
	fmt.Printf("  after ACK:     %s\n", conn.State())
	fmt.Printf("  can send:      %d bytes\n", conn.CanSend())
}
```

## Common Mistakes

### Wrong byte order in header fields

Wrong: `h.TotalLen = uint16(b[2]) | uint16(b[3])<<8` (little-endian read of a big-endian field).

What happens: the IP total-length field is read as byte-swapped, producing a nonsensical datagram length. Every comparison against the actual payload length fails. Packets are silently dropped or trigger a panic on slice bounds.

Fix: always use `binary.BigEndian.Uint16(b[2:4])` for network-byte-order fields. The `encoding/binary` package exists precisely to make this explicit. Every field in Ethernet, ARP, IPv4, ICMP, and TCP is big-endian.

### Forgetting the TCP pseudo-header in the checksum

Wrong: computing `InternetChecksum(tcpHeader)` without the 12-byte pseudo-header.

What happens: the checksum value is wrong by a fixed offset (the contribution of the pseudo-header). The Linux kernel drops the segment with a checksum error, which appears in Wireshark as `[TCP CHECKSUM INCORRECT]`.

Fix: always prepend the pseudo-header (src IP, dst IP, zero, proto, TCP length) before calling `InternetChecksum`. The `TCPChecksum` helper in `checksum.go` does this automatically.

### Treating SYN and FIN as zero-length data

Wrong: after processing a SYN, setting `rcvNXT = seg.SeqNum` instead of `seg.SeqNum + 1`.

What happens: the SYN-ACK acknowledges the wrong sequence number. The client sees its SYN not acknowledged, retransmits, and the handshake never completes.

Fix: SYN and FIN each consume one sequence number even though they carry no payload (RFC 793 §3.3). Always add 1 when acknowledging a SYN or FIN.

### Not protecting the Conn state machine with a mutex

Wrong: accessing `conn.state` from the dispatch goroutine and a separate send goroutine without synchronization.

What happens: data races that show up under `-race` or intermittent panics from reading a half-written state value. The TCP state machine is small enough that a single `sync.Mutex` per `Conn` is the correct and sufficient approach.

Fix: all state reads and writes happen inside a `c.mu.Lock()` / `c.mu.Unlock()` pair, as shown in `Conn.RecvSYN` and `Conn.RecvACK`.

### Ignoring duplicate ACKs

Wrong: returning `ErrInvalidSegment` when `ackNum == sndUNA`.

What happens: a legitimate duplicate ACK (from a delayed packet) causes the connection to error out. Three duplicate ACKs are the fast-retransmit signal; any duplicate ACK is valid and must be processed silently.

Fix: treat `ackNum == sndUNA` as a duplicate ACK. Update the window only and return nil, as in `RecvDataACK`.

## Verification

From `~/go-exercises/netstack`, run the pure-Go layer tests (no TAP device required, any OS):

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -count=1 -race ./...
```

The TAP device, the full integration, and the HTTP/1.0 server require Linux and root privileges. On a Linux host or VM:

```bash
go build ./...
sudo ./netstack &
sudo ip addr add 10.0.0.1/24 dev tap0
sudo ip link set tap0 up

# Layer 2: ARP resolution
arping -I tap0 10.0.0.2

# Layer 3: ICMP ping
ping -c 4 10.0.0.2

# Layer 4: TCP handshake (use Wireshark to inspect SYN/SYN-ACK/ACK)
curl -v http://10.0.0.2/

# Checksum correctness: Wireshark must show [correct] for every frame
```

Add `TestTCPRecvDataACKRejectsOutOfRange`: call `RecvDataACK` with an `ackNum` beyond `sndNXT` and assert `errors.Is(err, ErrInvalidSegment)`.

## Summary

- The TAP device (`/dev/net/tun`, `TUNSETIFF`, `IFF_TAP|IFF_NO_PI`) delivers raw Ethernet frames to a userspace process. No socket abstraction; no OS parsing.
- All network-byte-order fields must be read and written with `encoding/binary.BigEndian`; using host byte order is a silent bug.
- The RFC 1071 Internet Checksum is used identically for IPv4, ICMP, and TCP; TCP adds a 12-byte pseudo-header before running the same algorithm.
- The TCP state machine (LISTEN → SYN_RECEIVED → ESTABLISHED → CLOSE_WAIT → ...) is correct only if SYN and FIN each consume one sequence number.
- The sliding window (`sndWND - (sndNXT - sndUNA)`) determines how many bytes may be in flight at any moment; sequence number arithmetic is modular (`uint32` wraparound is intentional).
- Protect all `Conn` state with a mutex; dispatch and send paths run concurrently.
- Test the pure-Go layers (parsing, checksum, state machine) offline with `go test -race`; run the integration (ARP, ICMP, TCP) on Linux with Wireshark verification.

## What's Next

Next: [Go Language Server (LSP)](../10-go-language-server-lsp/10-go-language-server-lsp.md).

## Resources

- [RFC 793: Transmission Control Protocol](https://datatracker.ietf.org/doc/html/rfc793) — the definitive TCP specification; §3.2 (state machine) and §3.3 (sequence variables) are essential
- [RFC 826: ARP](https://datatracker.ietf.org/doc/html/rfc826) and [RFC 791: IPv4](https://datatracker.ietf.org/doc/html/rfc791) — full header layouts and field semantics
- [RFC 1071: Computing the Internet Checksum](https://datatracker.ietf.org/doc/html/rfc1071) — the algorithm and its carry-folding rationale
- [Linux TUN/TAP documentation](https://www.kernel.org/doc/html/latest/networking/tuntap.html) — `TUNSETIFF` flags, IFF_TAP vs IFF_TUN, packet-info header
- [google/gvisor pkg/tcpip](https://github.com/google/gvisor/tree/master/pkg/tcpip) — production userspace TCP/IP stack in Go; the reference for how `Conn`-level state machines and checksum helpers are structured at scale
