# 8. Raw Socket Packet Capture

Capturing raw packets in Go requires three skills that reinforce each other: opening an `AF_PACKET` socket on Linux and enabling promiscuous mode, parsing multi-layer protocol headers at the byte level using network byte order rules, and attaching a classic BPF filter in the kernel to drop unwanted frames before they reach userspace. The difficulty is that all three are tightly coupled — the wrong byte offset or a missing `htons` call produces corrupt data with no compiler error. This lesson builds each layer independently so the pure parsing code (testable on any platform) is clearly separated from the Linux-only socket layer.

The module structure:

```text
capture/
  go.mod
  frame.go         -- Ethernet/IPv4/TCP/UDP/ICMP types and parsers (pure, all platforms)
  checksum.go      -- ones'-complement checksum verification (pure, all platforms)
  pcap.go          -- pcap file writer (pure, all platforms)
  socket.go        -- AF_PACKET raw socket, promiscuous mode (//go:build linux)
  bpf.go           -- classic BPF filter attachment (//go:build linux)
  frame_test.go    -- tests for pure parsing functions
  pcap_test.go     -- tests for the pcap writer
  cmd/demo/main.go -- runnable demo (//go:build linux)
```

## Concepts

### AF_PACKET and the Linux Packet Socket

Linux exposes raw network access through the `AF_PACKET` socket family. A `SOCK_RAW` socket with protocol `ETH_P_ALL` (value 0x0003) receives every complete Ethernet frame on an interface before the kernel demultiplexes it to a higher-layer socket. The socket delivers:

```text
[dst MAC 6B][src MAC 6B][EtherType 2B][payload ...]
```

Two caveats apply immediately. First, the protocol argument to `socket(2)` must be in network byte order (big-endian), so `ETH_P_ALL` (0x0003) becomes 0x0300 on little-endian hardware. Go has no built-in `htons`; the conversion is a byte-swap:

```go
func htons(n uint16) uint16 { return (n >> 8) | (n << 8) }
```

Second, without promiscuous mode the NIC hardware-filters frames not addressed to the local MAC, so only frames sent to or from the machine arrive. Promiscuous mode is enabled with `setsockopt(PACKET_ADD_MEMBERSHIP, PACKET_MR_PROMISC)`. Both the socket and promiscuous mode require `CAP_NET_RAW` or root privileges.

### Network Byte Order and Header Parsing

Every multi-byte field in Ethernet, IP, TCP, and UDP headers arrives in network byte order (big-endian). The rule is simple: always use `encoding/binary.BigEndian.Uint16` or `Uint32`. Never cast a `[]byte` slice to an integer type — that violates Go's aliasing rules and produces wrong results on big-endian hardware.

Key header offsets (from the start of the header, in bytes):

| Field | Header | Offset | Width |
|---|---|---|---|
| EtherType | Ethernet | 12 | 2 |
| VLAN ID | 802.1Q (inner) | 14 + 2 | 2 |
| IHL (header length) | IPv4 | 0 (lower nibble × 4) | — |
| Protocol | IPv4 | 9 | 1 |
| Src IP | IPv4 | 12 | 4 |
| Dst IP | IPv4 | 16 | 4 |
| IP protocol at Ethernet offset | Ethernet + IP | 23 | 1 |
| Src Port | TCP / UDP | 0 | 2 |
| Data offset | TCP | 12 (upper nibble × 4) | — |
| Flags | TCP | 13 | 1 |

The IPv4 IHL field is the lower nibble of byte 0, expressed in 32-bit units. Header length in bytes = `(b[0] & 0x0F) * 4`. Minimum is 20; maximum is 60 (with options).

### IPv4 Header Checksum

The checksum covers only the IPv4 header. The algorithm is the ones' complement of the ones' complement sum of all 16-bit words, with the checksum field treated as zero during computation. A received frame is valid if summing all 16-bit words of the header — including the stored checksum field — produces 0xFFFF after carry folding:

```text
sum = 0
for each 16-bit word w in header:
    sum += w            // uint32 accumulator
    fold carry: sum = (sum & 0xffff) + (sum >> 16)  // until no carry
valid when sum == 0xffff
```

This works because the stored checksum is the ones' complement of the partial sum, so adding it back produces all ones (0xffff).

### Classic BPF Filters

The Linux kernel runs a BPF virtual machine against every incoming frame and delivers it to userspace only when the program returns non-zero. Attaching a tight filter with `setsockopt(SOL_SOCKET, SO_ATTACH_FILTER)` drops non-matching frames entirely in kernel space, which drastically reduces copies and `recvfrom` calls at high packet rates.

A classic BPF program is a slice of `syscall.SockFilter` instructions. Each instruction encodes an opcode (`Code`), two jump-offset fields (`Jt` for true, `Jf` for false), and a constant (`K`). Useful opcodes:

| Opcode | Value | Meaning |
|---|---|---|
| `LD h abs` | 0x28 | load halfword at absolute offset into accumulator |
| `LD b abs` | 0x30 | load byte at absolute offset |
| `JEQ K` | 0x15 | jump if accumulator == K |
| `RET K` | 0x06 | return K (0 = drop; 0xffff = accept all bytes) |

A TCP-only filter checks EtherType at offset 12 == 0x0800, then the IP protocol byte at offset 23 (Ethernet 14 + IP protocol 9) == 6 (TCP).

### The pcap File Format

pcap is a flat binary format understood by Wireshark and tcpdump. All fields are in little-endian byte order when the magic number is `0xa1b2c3d4`. The layout is:

```text
Global header (24 bytes):
  magic:      0xa1b2c3d4  (uint32 LE)
  ver_major:  2           (uint16 LE)
  ver_minor:  4           (uint16 LE)
  thiszone:   0           (int32  LE)
  sigfigs:    0           (uint32 LE)
  snaplen:    65535       (uint32 LE)
  network:    1           (uint32 LE — LINKTYPE_ETHERNET)

Per-packet record header (16 bytes) + raw frame data:
  ts_sec:   uint32 LE  -- seconds since Unix epoch
  ts_usec:  uint32 LE  -- microseconds
  incl_len: uint32 LE  -- bytes in the file
  orig_len: uint32 LE  -- bytes on the wire
```

`incl_len` equals `orig_len` unless the frame was truncated to `snaplen`.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/capture/cmd/demo
cd ~/go-exercises/capture
go mod init example.com/capture
```

Create `go.mod`:

```go
module example.com/capture

go 1.26
```

The files `frame.go`, `checksum.go`, `pcap.go`, and their tests compile and run on any platform. The files `socket.go`, `bpf.go`, and `cmd/demo/main.go` carry `//go:build linux` and are compiled only on Linux.

### Exercise 1: Protocol Header Types and Parsers

Create `frame.go`:

```go
package capture

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// EtherType values for common protocols.
const (
	EtherTypeIPv4 = 0x0800
	EtherTypeARP  = 0x0806
	EtherTypeVLAN = 0x8100
	EtherTypeIPv6 = 0x86DD
)

// IP protocol numbers.
const (
	ProtoICMP = 1
	ProtoTCP  = 6
	ProtoUDP  = 17
)

// TCP flag bitmask constants.
const (
	FlagFIN = 0x01
	FlagSYN = 0x02
	FlagRST = 0x04
	FlagPSH = 0x08
	FlagACK = 0x10
	FlagURG = 0x20
)

var (
	ErrFrameTooShort = errors.New("capture: frame too short")
	ErrBadIPVersion  = errors.New("capture: not an IPv4 header")
)

// EthernetHeader holds the decoded Ethernet II (and optionally 802.1Q) header.
type EthernetHeader struct {
	Dst       net.HardwareAddr
	Src       net.HardwareAddr
	EtherType uint16
	VLANID    uint16 // non-zero when a VLAN tag was present
}

// IPv4Header holds the decoded IPv4 header fields.
// Src and Dst are copies of the address bytes; they are safe to retain after
// the original frame buffer is recycled.
type IPv4Header struct {
	Version    uint8
	IHL        uint8 // header length in bytes (IHL field × 4)
	DSCP       uint8
	TotalLen   uint16
	ID         uint16
	DontFrag   bool
	MoreFrags  bool
	FragOffset uint16
	TTL        uint8
	Protocol   uint8
	Checksum   uint16
	Src        net.IP
	Dst        net.IP
}

// TCPHeader holds the decoded TCP header fields.
type TCPHeader struct {
	SrcPort  uint16
	DstPort  uint16
	Seq      uint32
	Ack      uint32
	DataOff  uint8 // data offset in bytes (data offset field × 4)
	Flags    uint8
	Window   uint16
	Checksum uint16
	Urgent   uint16
}

// UDPHeader holds the decoded UDP header fields.
type UDPHeader struct {
	SrcPort  uint16
	DstPort  uint16
	Length   uint16
	Checksum uint16
}

// ICMPHeader holds the first four bytes of an ICMP message.
type ICMPHeader struct {
	Type     uint8
	Code     uint8
	Checksum uint16
}

// ParseEthernet decodes the Ethernet II header from b.
// VLAN-tagged frames (EtherType 0x8100) are unwrapped one level: the inner
// EtherType is stored in h.EtherType and the VLAN ID in h.VLANID.
// The returned payload starts immediately after the (optionally VLAN-extended)
// header.
func ParseEthernet(b []byte) (h EthernetHeader, payload []byte, err error) {
	if len(b) < 14 {
		return h, nil, fmt.Errorf("%w: ethernet needs 14 bytes, got %d", ErrFrameTooShort, len(b))
	}
	// Copy MAC addresses so h is safe after b is recycled.
	dst := make(net.HardwareAddr, 6)
	src := make(net.HardwareAddr, 6)
	copy(dst, b[0:6])
	copy(src, b[6:12])
	h.Dst = dst
	h.Src = src
	h.EtherType = binary.BigEndian.Uint16(b[12:14])
	payload = b[14:]

	if h.EtherType == EtherTypeVLAN {
		if len(payload) < 4 {
			return h, nil, fmt.Errorf("%w: 802.1Q tag needs 4 more bytes", ErrFrameTooShort)
		}
		tci := binary.BigEndian.Uint16(payload[0:2])
		h.VLANID = tci & 0x0FFF
		h.EtherType = binary.BigEndian.Uint16(payload[2:4])
		payload = payload[4:]
	}
	return h, payload, nil
}

// ParseIPv4 decodes an IPv4 header from b.
// The returned payload starts at the first byte after the header (at offset IHL).
// ParseIPv4 does not validate the checksum; call VerifyIPv4Checksum separately.
func ParseIPv4(b []byte) (h IPv4Header, payload []byte, err error) {
	if len(b) < 20 {
		return h, nil, fmt.Errorf("%w: IPv4 needs 20 bytes, got %d", ErrFrameTooShort, len(b))
	}
	h.Version = b[0] >> 4
	if h.Version != 4 {
		return h, nil, fmt.Errorf("%w: version field = %d", ErrBadIPVersion, h.Version)
	}
	ihl := int(b[0]&0x0F) * 4
	if ihl < 20 || len(b) < ihl {
		return h, nil, fmt.Errorf("%w: IHL=%d but buffer is %d bytes", ErrFrameTooShort, ihl, len(b))
	}
	h.IHL = uint8(ihl)
	h.DSCP = b[1] >> 2
	h.TotalLen = binary.BigEndian.Uint16(b[2:4])
	h.ID = binary.BigEndian.Uint16(b[4:6])
	flagsFrag := binary.BigEndian.Uint16(b[6:8])
	h.DontFrag = flagsFrag&0x4000 != 0
	h.MoreFrags = flagsFrag&0x2000 != 0
	h.FragOffset = flagsFrag & 0x1FFF
	h.TTL = b[8]
	h.Protocol = b[9]
	h.Checksum = binary.BigEndian.Uint16(b[10:12])
	// Copy IPs so they are safe after b is recycled.
	h.Src = make(net.IP, 4)
	h.Dst = make(net.IP, 4)
	copy(h.Src, b[12:16])
	copy(h.Dst, b[16:20])
	payload = b[ihl:]
	return h, payload, nil
}

// ParseTCP decodes a TCP header from b.
// The returned payload starts at the first byte of the TCP data segment
// (at offset DataOff).
func ParseTCP(b []byte) (h TCPHeader, payload []byte, err error) {
	if len(b) < 20 {
		return h, nil, fmt.Errorf("%w: TCP needs 20 bytes, got %d", ErrFrameTooShort, len(b))
	}
	h.SrcPort = binary.BigEndian.Uint16(b[0:2])
	h.DstPort = binary.BigEndian.Uint16(b[2:4])
	h.Seq = binary.BigEndian.Uint32(b[4:8])
	h.Ack = binary.BigEndian.Uint32(b[8:12])
	dataOff := int(b[12]>>4) * 4
	if dataOff < 20 || len(b) < dataOff {
		return h, nil, fmt.Errorf("%w: TCP data offset=%d but buffer is %d bytes", ErrFrameTooShort, dataOff, len(b))
	}
	h.DataOff = uint8(dataOff)
	h.Flags = b[13]
	h.Window = binary.BigEndian.Uint16(b[14:16])
	h.Checksum = binary.BigEndian.Uint16(b[16:18])
	h.Urgent = binary.BigEndian.Uint16(b[18:20])
	payload = b[dataOff:]
	return h, payload, nil
}

// ParseUDP decodes a UDP header from b.
func ParseUDP(b []byte) (h UDPHeader, payload []byte, err error) {
	if len(b) < 8 {
		return h, nil, fmt.Errorf("%w: UDP needs 8 bytes, got %d", ErrFrameTooShort, len(b))
	}
	h.SrcPort = binary.BigEndian.Uint16(b[0:2])
	h.DstPort = binary.BigEndian.Uint16(b[2:4])
	h.Length = binary.BigEndian.Uint16(b[4:6])
	h.Checksum = binary.BigEndian.Uint16(b[6:8])
	payload = b[8:]
	return h, payload, nil
}

// ParseICMP decodes the fixed four-byte ICMP header from b.
// The meaning of the remaining bytes depends on Type and Code.
func ParseICMP(b []byte) (h ICMPHeader, payload []byte, err error) {
	if len(b) < 4 {
		return h, nil, fmt.Errorf("%w: ICMP needs 4 bytes, got %d", ErrFrameTooShort, len(b))
	}
	h.Type = b[0]
	h.Code = b[1]
	h.Checksum = binary.BigEndian.Uint16(b[2:4])
	payload = b[4:]
	return h, payload, nil
}
```

### Exercise 2: Checksum Verification

Create `checksum.go`:

```go
package capture

import (
	"encoding/binary"
	"net"
)

// VerifyIPv4Checksum returns true when the stored checksum in an IPv4 header
// is correct. header must contain exactly IHL bytes (the raw header, no payload).
// The algorithm sums all 16-bit words including the checksum field; a valid
// header produces a carry-folded sum of 0xffff.
func VerifyIPv4Checksum(header []byte) bool {
	return onesComplementSum(header) == 0xffff
}

// VerifyTCPChecksum returns true when the TCP segment checksum is valid.
// srcIP and dstIP are 4-byte IPv4 addresses. segment is the complete TCP
// header plus TCP payload (not yet stripped of the TCP header).
// The TCP pseudo-header (srcIP, dstIP, zero, protocol=6, tcp-length) is
// prepended before summing, per RFC 793 §3.1.
func VerifyTCPChecksum(srcIP, dstIP net.IP, segment []byte) bool {
	tcpLen := uint16(len(segment))
	pseudo := make([]byte, 12+len(segment))
	copy(pseudo[0:4], srcIP.To4())
	copy(pseudo[4:8], dstIP.To4())
	pseudo[8] = 0
	pseudo[9] = ProtoTCP
	binary.BigEndian.PutUint16(pseudo[10:12], tcpLen)
	copy(pseudo[12:], segment)
	return onesComplementSum(pseudo) == 0xffff
}

// onesComplementSum returns the carry-folded ones' complement sum of all
// 16-bit words in b. If len(b) is odd, the last byte is zero-padded.
// A uint32 accumulator is used to hold carries before folding.
func onesComplementSum(b []byte) uint16 {
	var sum uint32
	for len(b) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(b[:2]))
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8 // zero-pad the missing byte
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return uint16(sum)
}
```

### Exercise 3: pcap File Writer

Create `pcap.go`:

```go
package capture

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

const (
	pcapMagic   uint32 = 0xa1b2c3d4
	pcapVerMaj  uint16 = 2
	pcapVerMin  uint16 = 4
	pcapNetwork uint32 = 1 // LINKTYPE_ETHERNET
)

// Writer writes raw packets in the libpcap file format, readable by
// Wireshark and tcpdump. All fields are written in little-endian order to
// match the 0xa1b2c3d4 magic number.
type Writer struct {
	w       io.Writer
	snaplen uint32
}

// WriterOption configures a Writer.
type WriterOption func(*Writer)

// WithSnapLen sets the maximum number of bytes stored per packet.
// Packets longer than snaplen are truncated; orig_len still records the
// true on-wire length. The default is 65535.
func WithSnapLen(n uint32) WriterOption {
	return func(w *Writer) {
		if n > 0 {
			w.snaplen = n
		}
	}
}

// NewWriter writes the 24-byte pcap global header to w and returns a Writer.
// Every subsequent call to WritePacket appends a packet record.
func NewWriter(w io.Writer, opts ...WriterOption) (*Writer, error) {
	pw := &Writer{w: w, snaplen: 65535}
	for _, opt := range opts {
		opt(pw)
	}
	var hdr [24]byte
	binary.LittleEndian.PutUint32(hdr[0:4], pcapMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], pcapVerMaj)
	binary.LittleEndian.PutUint16(hdr[6:8], pcapVerMin)
	binary.LittleEndian.PutUint32(hdr[8:12], 0)  // thiszone
	binary.LittleEndian.PutUint32(hdr[12:16], 0) // sigfigs
	binary.LittleEndian.PutUint32(hdr[16:20], pw.snaplen)
	binary.LittleEndian.PutUint32(hdr[20:24], pcapNetwork)
	if _, err := w.Write(hdr[:]); err != nil {
		return nil, fmt.Errorf("capture: pcap global header: %w", err)
	}
	return pw, nil
}

// WritePacket appends a packet record to the pcap stream.
// ts is the capture timestamp; data is the raw Ethernet frame.
// data is truncated to snaplen bytes in the file, but orig_len records the
// full on-wire length.
func (pw *Writer) WritePacket(ts time.Time, data []byte) error {
	origLen := uint32(len(data))
	capLen := origLen
	if capLen > pw.snaplen {
		capLen = pw.snaplen
	}
	var rec [16]byte
	binary.LittleEndian.PutUint32(rec[0:4], uint32(ts.Unix()))
	binary.LittleEndian.PutUint32(rec[4:8], uint32(ts.Nanosecond()/1e3))
	binary.LittleEndian.PutUint32(rec[8:12], capLen)
	binary.LittleEndian.PutUint32(rec[12:16], origLen)
	if _, err := pw.w.Write(rec[:]); err != nil {
		return fmt.Errorf("capture: pcap record header: %w", err)
	}
	if _, err := pw.w.Write(data[:capLen]); err != nil {
		return fmt.Errorf("capture: pcap packet data: %w", err)
	}
	return nil
}
```

### Exercise 4: Raw Socket and Promiscuous Mode (Linux only)

Create `socket.go`:

```go
//go:build linux

package capture

import (
	"fmt"
	"net"
	"syscall"
)

// htons converts a uint16 from host byte order to network byte order.
// AF_PACKET sockets require the protocol argument in network byte order.
func htons(n uint16) uint16 { return (n >> 8) | (n << 8) }

// Capturer captures raw Ethernet frames from a network interface using
// an AF_PACKET/SOCK_RAW socket. Requires CAP_NET_RAW or root privileges.
type Capturer struct {
	fd      int
	iface   *net.Interface
	snaplen int
}

// CaptureOption configures a Capturer.
type CaptureOption func(*Capturer) error

// WithCaptureSnapLen sets the maximum bytes read per packet (default 65535).
func WithCaptureSnapLen(n int) CaptureOption {
	return func(c *Capturer) error {
		if n < 1 || n > 65535 {
			return fmt.Errorf("capture: snaplen %d out of [1, 65535]", n)
		}
		c.snaplen = n
		return nil
	}
}

// New opens an AF_PACKET raw socket bound to ifaceName, enables promiscuous
// mode, and returns a Capturer. Call Close when done.
func New(ifaceName string, opts ...CaptureOption) (*Capturer, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("capture: interface %q: %w", ifaceName, err)
	}

	fd, err := syscall.Socket(
		syscall.AF_PACKET,
		syscall.SOCK_RAW,
		int(htons(syscall.ETH_P_ALL)),
	)
	if err != nil {
		return nil, fmt.Errorf("capture: socket: %w", err)
	}

	ll := syscall.SockaddrLinklayer{
		Protocol: htons(syscall.ETH_P_ALL),
		Ifindex:  iface.Index,
	}
	if err := syscall.Bind(fd, &ll); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("capture: bind: %w", err)
	}

	if err := setPromisc(fd, iface.Index, true); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("capture: promisc: %w", err)
	}

	c := &Capturer{fd: fd, iface: iface, snaplen: 65535}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			syscall.Close(fd)
			return nil, err
		}
	}
	return c, nil
}

// setPromisc adds (on=true) or removes (on=false) PACKET_MR_PROMISC
// membership on the interface identified by ifindex.
func setPromisc(fd, ifindex int, on bool) error {
	mreq := syscall.PacketMreq{
		Ifindex: int32(ifindex),
		Type:    syscall.PACKET_MR_PROMISC,
	}
	opt := syscall.PACKET_ADD_MEMBERSHIP
	if !on {
		opt = syscall.PACKET_DROP_MEMBERSHIP
	}
	return syscall.SetsockoptPacketMreq(fd, syscall.SOL_PACKET, opt, &mreq)
}

// Capture reads one raw Ethernet frame into buf and returns the number of
// bytes written. buf should be at least SnapLen() bytes. Blocks until a
// frame arrives or the socket is closed.
func (c *Capturer) Capture(buf []byte) (int, error) {
	n, _, err := syscall.Recvfrom(c.fd, buf, 0)
	if err != nil {
		return 0, fmt.Errorf("capture: recvfrom: %w", err)
	}
	return n, nil
}

// SnapLen returns the configured maximum capture length in bytes.
func (c *Capturer) SnapLen() int { return c.snaplen }

// Close removes promiscuous mode and closes the underlying socket.
func (c *Capturer) Close() error {
	_ = setPromisc(c.fd, c.iface.Index, false)
	if err := syscall.Close(c.fd); err != nil {
		return fmt.Errorf("capture: close: %w", err)
	}
	return nil
}
```

### Exercise 5: BPF Filter Attachment (Linux only)

Create `bpf.go`:

```go
//go:build linux

package capture

import (
	"fmt"
	"syscall"
)

// TCPFilter is a classic BPF program that accepts only IPv4/TCP frames.
// Attach it before the capture loop to drop all other frames in the kernel,
// reducing the number of syscalls and memory copies at high packet rates.
//
// Instruction breakdown:
//
//	ldh  [12]      -- load EtherType halfword
//	jeq  0x0800    -- skip to ret#0 if not IPv4
//	ldb  [23]      -- load IP protocol byte (Ethernet 14 + IP.protocol offset 9)
//	jeq  6         -- skip to ret#0 if not TCP
//	ret  0xffff    -- accept (capture all bytes)
//	ret  0          -- drop
var TCPFilter = []syscall.SockFilter{
	{Code: 0x28, Jt: 0, Jf: 0, K: 0x0000000c},
	{Code: 0x15, Jt: 0, Jf: 3, K: 0x00000800},
	{Code: 0x30, Jt: 0, Jf: 0, K: 0x00000017},
	{Code: 0x15, Jt: 0, Jf: 1, K: 0x00000006},
	{Code: 0x06, Jt: 0, Jf: 0, K: 0x0000ffff},
	{Code: 0x06, Jt: 0, Jf: 0, K: 0x00000000},
}

// UDPFilter is a classic BPF program that accepts only IPv4/UDP frames.
// The structure mirrors TCPFilter; protocol byte at offset 23 is compared
// to 17 (0x11) instead of 6.
var UDPFilter = []syscall.SockFilter{
	{Code: 0x28, Jt: 0, Jf: 0, K: 0x0000000c},
	{Code: 0x15, Jt: 0, Jf: 3, K: 0x00000800},
	{Code: 0x30, Jt: 0, Jf: 0, K: 0x00000017},
	{Code: 0x15, Jt: 0, Jf: 1, K: 0x00000011},
	{Code: 0x06, Jt: 0, Jf: 0, K: 0x0000ffff},
	{Code: 0x06, Jt: 0, Jf: 0, K: 0x00000000},
}

// AttachFilter installs a classic BPF filter on c's socket with
// setsockopt(SOL_SOCKET, SO_ATTACH_FILTER). After this call the kernel
// evaluates the program against every incoming frame and delivers only
// those for which the program returns non-zero.
func (c *Capturer) AttachFilter(filter []syscall.SockFilter) error {
	if len(filter) == 0 {
		return fmt.Errorf("capture: AttachFilter: empty program")
	}
	fp := syscall.SockFprog{
		Len:    uint16(len(filter)),
		Filter: &filter[0],
	}
	if err := syscall.SetsockoptSockFprog(
		c.fd, syscall.SOL_SOCKET, syscall.SO_ATTACH_FILTER, &fp,
	); err != nil {
		return fmt.Errorf("capture: AttachFilter: %w", err)
	}
	return nil
}
```

### Exercise 6: Tests

The parsers and the pcap writer operate on plain byte slices and have no platform dependencies. These tests run offline on any OS.

Create `frame_test.go`:

```go
package capture

import (
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestParseEthernet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []byte
		wantDst  string
		wantSrc  string
		wantType uint16
		wantVLAN uint16
		wantErr  error
	}{
		{
			name: "plain IPv4",
			input: []byte{
				0xff, 0xff, 0xff, 0xff, 0xff, 0xff, // dst: broadcast
				0x00, 0x11, 0x22, 0x33, 0x44, 0x55, // src
				0x08, 0x00, // EtherType: IPv4
				0x45, 0x00, // start of payload
			},
			wantDst:  "ff:ff:ff:ff:ff:ff",
			wantSrc:  "00:11:22:33:44:55",
			wantType: EtherTypeIPv4,
		},
		{
			name: "VLAN tagged frame",
			input: []byte{
				0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
				0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
				0x81, 0x00, // outer EtherType: 802.1Q
				0x00, 0x64, // TCI: PCP=0 DEI=0 VID=100
				0x08, 0x00, // inner EtherType: IPv4
				0x45, // payload
			},
			wantDst:  "ff:ff:ff:ff:ff:ff",
			wantSrc:  "aa:bb:cc:dd:ee:ff",
			wantType: EtherTypeIPv4,
			wantVLAN: 100,
		},
		{
			name:    "frame too short",
			input:   []byte{0x00, 0x01, 0x02},
			wantErr: ErrFrameTooShort,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, _, err := ParseEthernet(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want wrapping %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h.Dst.String() != tc.wantDst {
				t.Errorf("Dst = %s, want %s", h.Dst, tc.wantDst)
			}
			if h.Src.String() != tc.wantSrc {
				t.Errorf("Src = %s, want %s", h.Src, tc.wantSrc)
			}
			if h.EtherType != tc.wantType {
				t.Errorf("EtherType = 0x%04x, want 0x%04x", h.EtherType, tc.wantType)
			}
			if h.VLANID != tc.wantVLAN {
				t.Errorf("VLANID = %d, want %d", h.VLANID, tc.wantVLAN)
			}
		})
	}
}

// ipv4Header is a valid 20-byte IPv4 header:
// src=192.168.1.1, dst=192.168.1.2, TTL=64, proto=TCP, DF set.
// Checksum 0x0b9b is the ones' complement of the header word sum.
var ipv4Header = []byte{
	0x45, 0x00, 0x00, 0x3c, 0xab, 0xcd,
	0x40, 0x00, 0x40, 0x06, 0x0b, 0x9b,
	0xc0, 0xa8, 0x01, 0x01,
	0xc0, 0xa8, 0x01, 0x02,
}

func TestParseIPv4(t *testing.T) {
	t.Parallel()

	t.Run("valid header", func(t *testing.T) {
		t.Parallel()
		h, payload, err := ParseIPv4(ipv4Header)
		if err != nil {
			t.Fatalf("ParseIPv4: %v", err)
		}
		if h.Version != 4 {
			t.Errorf("Version = %d, want 4", h.Version)
		}
		if h.IHL != 20 {
			t.Errorf("IHL = %d, want 20", h.IHL)
		}
		if h.Protocol != ProtoTCP {
			t.Errorf("Protocol = %d, want %d (TCP)", h.Protocol, ProtoTCP)
		}
		if !h.DontFrag {
			t.Error("DontFrag should be set (flags byte = 0x40)")
		}
		if !h.Src.Equal(net.IP{192, 168, 1, 1}) {
			t.Errorf("Src = %s, want 192.168.1.1", h.Src)
		}
		if !h.Dst.Equal(net.IP{192, 168, 1, 2}) {
			t.Errorf("Dst = %s, want 192.168.1.2", h.Dst)
		}
		if len(payload) != 0 {
			t.Errorf("payload len = %d, want 0 (header-only input)", len(payload))
		}
	})

	t.Run("too short", func(t *testing.T) {
		t.Parallel()
		_, _, err := ParseIPv4(ipv4Header[:10])
		if !errors.Is(err, ErrFrameTooShort) {
			t.Fatalf("err = %v, want ErrFrameTooShort", err)
		}
	})

	t.Run("wrong IP version", func(t *testing.T) {
		t.Parallel()
		bad := make([]byte, 20)
		copy(bad, ipv4Header)
		bad[0] = (bad[0] & 0x0F) | 0x60 // set version = 6
		_, _, err := ParseIPv4(bad)
		if !errors.Is(err, ErrBadIPVersion) {
			t.Fatalf("err = %v, want ErrBadIPVersion", err)
		}
	})
}

func TestParseTCP(t *testing.T) {
	t.Parallel()

	// TCP SYN: src=12345, dst=80, seq=0x01234567, data-offset=5 (20 bytes), flags=SYN.
	synBytes := []byte{
		0x30, 0x39, 0x00, 0x50, // src=12345 dst=80
		0x01, 0x23, 0x45, 0x67, // seq
		0x00, 0x00, 0x00, 0x00, // ack
		0x50, 0x02, // data offset=5, flags=SYN
		0xfa, 0xf0, // window=64240
		0x00, 0x00, // checksum (not verified here)
		0x00, 0x00, // urgent
	}

	t.Run("valid SYN", func(t *testing.T) {
		t.Parallel()
		h, payload, err := ParseTCP(synBytes)
		if err != nil {
			t.Fatalf("ParseTCP: %v", err)
		}
		if h.SrcPort != 12345 {
			t.Errorf("SrcPort = %d, want 12345", h.SrcPort)
		}
		if h.DstPort != 80 {
			t.Errorf("DstPort = %d, want 80", h.DstPort)
		}
		if h.Seq != 0x01234567 {
			t.Errorf("Seq = 0x%08x, want 0x01234567", h.Seq)
		}
		if h.Flags&FlagSYN == 0 {
			t.Error("SYN flag not set")
		}
		if h.Flags&FlagACK != 0 {
			t.Error("ACK flag should not be set in a bare SYN")
		}
		if h.DataOff != 20 {
			t.Errorf("DataOff = %d, want 20", h.DataOff)
		}
		if len(payload) != 0 {
			t.Errorf("payload len = %d, want 0", len(payload))
		}
	})

	t.Run("too short", func(t *testing.T) {
		t.Parallel()
		_, _, err := ParseTCP(synBytes[:10])
		if !errors.Is(err, ErrFrameTooShort) {
			t.Fatalf("err = %v, want ErrFrameTooShort", err)
		}
	})
}

func TestVerifyIPv4Checksum(t *testing.T) {
	t.Parallel()

	if !VerifyIPv4Checksum(ipv4Header) {
		t.Fatal("expected valid checksum for ipv4Header fixture")
	}

	corrupted := make([]byte, len(ipv4Header))
	copy(corrupted, ipv4Header)
	corrupted[8]++ // increment TTL; invalidates checksum
	if VerifyIPv4Checksum(corrupted) {
		t.Fatal("expected invalid checksum after TTL corruption")
	}
}

func TestParseICMP(t *testing.T) {
	t.Parallel()

	// ICMP echo request: type=8, code=0, checksum=0x0000
	b := []byte{0x08, 0x00, 0x00, 0x00, 0xde, 0xad}
	h, payload, err := ParseICMP(b)
	if err != nil {
		t.Fatalf("ParseICMP: %v", err)
	}
	if h.Type != 8 || h.Code != 0 {
		t.Errorf("Type=%d Code=%d, want 8 0", h.Type, h.Code)
	}
	if len(payload) != 2 {
		t.Errorf("payload len = %d, want 2", len(payload))
	}
}

func TestParseUDP(t *testing.T) {
	t.Parallel()

	// UDP datagram: src=53, dst=1024, length=12, checksum=0
	b := []byte{0x00, 0x35, 0x04, 0x00, 0x00, 0x0c, 0x00, 0x00, 0xde, 0xad}
	h, payload, err := ParseUDP(b)
	if err != nil {
		t.Fatalf("ParseUDP: %v", err)
	}
	if h.SrcPort != 53 || h.DstPort != 1024 {
		t.Errorf("ports %d/%d, want 53/1024", h.SrcPort, h.DstPort)
	}
	if len(payload) != 2 {
		t.Errorf("payload len = %d, want 2", len(payload))
	}
}

// ExampleParseEthernet shows how to decode a broadcast Ethernet frame.
func ExampleParseEthernet() {
	frame := []byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, // dst: broadcast
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, // src
		0x08, 0x00, // EtherType: IPv4
		0x45, 0x00, // payload (first two bytes of an IP header)
	}
	h, _, err := ParseEthernet(frame)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("dst=%s src=%s type=0x%04x\n", h.Dst, h.Src, h.EtherType)
	// Output: dst=ff:ff:ff:ff:ff:ff src=00:11:22:33:44:55 type=0x0800
}

// ExampleVerifyIPv4Checksum shows checksum verification on a known-good header.
// The header encodes 192.168.1.1 -> 192.168.1.2, TTL=64, proto=TCP.
// The checksum field (bytes 10-11) is 0x0b9b, which is the ones' complement
// of the sum of the other nine 16-bit header words.
func ExampleVerifyIPv4Checksum() {
	header := []byte{
		0x45, 0x00, 0x00, 0x3c, 0xab, 0xcd,
		0x40, 0x00, 0x40, 0x06, 0x0b, 0x9b,
		0xc0, 0xa8, 0x01, 0x01,
		0xc0, 0xa8, 0x01, 0x02,
	}
	fmt.Println(VerifyIPv4Checksum(header))
	// Output: true
}
```

Your turn: add `TestVerifyIPv4ChecksumAllZeroChecksum` that creates a 20-byte header with the checksum field set to zero and asserts `VerifyIPv4Checksum` returns false (a zero checksum is never valid for any non-zero header).

Create `pcap_test.go`:

```go
package capture

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestNewWriterGlobalHeader(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	_, err := NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	b := buf.Bytes()
	if len(b) != 24 {
		t.Fatalf("global header length = %d, want 24", len(b))
	}
	magic := binary.LittleEndian.Uint32(b[0:4])
	if magic != 0xa1b2c3d4 {
		t.Errorf("magic = 0x%08x, want 0xa1b2c3d4", magic)
	}
	maj := binary.LittleEndian.Uint16(b[4:6])
	min := binary.LittleEndian.Uint16(b[6:8])
	if maj != 2 || min != 4 {
		t.Errorf("version = %d.%d, want 2.4", maj, min)
	}
	linkType := binary.LittleEndian.Uint32(b[20:24])
	if linkType != 1 {
		t.Errorf("link type = %d, want 1 (LINKTYPE_ETHERNET)", linkType)
	}
}

func TestWritePacket(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	pw, err := NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	pkt := []byte{0x01, 0x02, 0x03, 0x04}
	// Unix timestamp 1700000000 with 500000 microseconds
	ts := time.Unix(1700000000, 500000*1000)
	if err := pw.WritePacket(ts, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	b := buf.Bytes()
	// 24 (global header) + 16 (record header) + 4 (data) = 44
	if len(b) != 44 {
		t.Fatalf("total file length = %d, want 44", len(b))
	}

	rec := b[24:]
	tsSec := binary.LittleEndian.Uint32(rec[0:4])
	tsUsec := binary.LittleEndian.Uint32(rec[4:8])
	inclLen := binary.LittleEndian.Uint32(rec[8:12])
	origLen := binary.LittleEndian.Uint32(rec[12:16])

	if tsSec != 1700000000 {
		t.Errorf("ts_sec = %d, want 1700000000", tsSec)
	}
	if tsUsec != 500000 {
		t.Errorf("ts_usec = %d, want 500000", tsUsec)
	}
	if inclLen != 4 || origLen != 4 {
		t.Errorf("incl_len=%d orig_len=%d, want 4 4", inclLen, origLen)
	}
	if !bytes.Equal(rec[16:], pkt) {
		t.Errorf("packet data = %v, want %v", rec[16:], pkt)
	}
}

func TestWritePacketSnaplenTruncation(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	pw, err := NewWriter(&buf, WithSnapLen(10))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	pkt := bytes.Repeat([]byte{0xAA}, 100) // 100 bytes on wire
	if err := pw.WritePacket(time.Now(), pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	b := buf.Bytes()
	rec := b[24:]
	inclLen := binary.LittleEndian.Uint32(rec[8:12])
	origLen := binary.LittleEndian.Uint32(rec[12:16])

	if inclLen != 10 {
		t.Errorf("incl_len = %d, want 10 (snaplen)", inclLen)
	}
	if origLen != 100 {
		t.Errorf("orig_len = %d, want 100", origLen)
	}
	// File should be: 24 (global) + 16 (rec header) + 10 (truncated data) = 50
	if len(b) != 50 {
		t.Errorf("total file length = %d, want 50", len(b))
	}
}
```

### Exercise 7: Demo Program (Linux only)

Create `cmd/demo/main.go`:

```go
//go:build linux

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"example.com/capture"
)

func main() {
	iface := flag.String("i", "eth0", "network interface")
	outFile := flag.String("w", "", "write pcap to file (omit for display)")
	tcpOnly := flag.Bool("tcp", false, "attach TCP-only BPF filter")
	count := flag.Int("n", 0, "stop after N packets (0 = unlimited)")
	flag.Parse()

	c, err := capture.New(*iface)
	if err != nil {
		log.Fatalf("capture.New(%q): %v  (need CAP_NET_RAW or run as root)", *iface, err)
	}
	defer c.Close()

	if *tcpOnly {
		if err := c.AttachFilter(capture.TCPFilter); err != nil {
			log.Fatalf("AttachFilter: %v", err)
		}
		log.Println("BPF filter: IPv4/TCP only")
	}

	var pw *capture.Writer
	if *outFile != "" {
		f, err := os.Create(*outFile)
		if err != nil {
			log.Fatalf("create %s: %v", *outFile, err)
		}
		defer f.Close()
		pw, err = capture.NewWriter(f)
		if err != nil {
			log.Fatalf("NewWriter: %v", err)
		}
		log.Printf("Writing pcap to %s", *outFile)
	}

	buf := make([]byte, c.SnapLen())
	n := 0
	log.Printf("Capturing on %s — press Ctrl-C to stop", *iface)

	for {
		nr, err := c.Capture(buf)
		if err != nil {
			log.Printf("Capture: %v", err)
			continue
		}
		n++

		if pw != nil {
			if err := pw.WritePacket(time.Now(), buf[:nr]); err != nil {
				log.Printf("WritePacket: %v", err)
			}
		} else {
			printFrame(n, buf[:nr])
		}

		if *count > 0 && n >= *count {
			log.Printf("Captured %d packets.", n)
			return
		}
	}
}

// printFrame decodes and displays one Ethernet frame to stdout.
func printFrame(seq int, raw []byte) {
	eth, payload, err := capture.ParseEthernet(raw)
	if err != nil {
		fmt.Printf("[%d] (parse error: %v)\n", seq, err)
		return
	}

	switch eth.EtherType {
	case capture.EtherTypeIPv4:
		ip, transport, err := capture.ParseIPv4(payload)
		if err != nil {
			fmt.Printf("[%d] IPv4 (parse error: %v)\n", seq, err)
			return
		}
		checksumOK := capture.VerifyIPv4Checksum(payload[:ip.IHL])
		switch ip.Protocol {
		case capture.ProtoTCP:
			tcp, _, err := capture.ParseTCP(transport)
			if err == nil {
				fmt.Printf("[%d] TCP  %s:%-5d -> %s:%-5d [%s] seq=%-10d ck=%v\n",
					seq, ip.Src, tcp.SrcPort, ip.Dst, tcp.DstPort,
					flagStr(tcp.Flags), tcp.Seq, checksumOK)
			}
		case capture.ProtoUDP:
			udp, _, err := capture.ParseUDP(transport)
			if err == nil {
				fmt.Printf("[%d] UDP  %s:%-5d -> %s:%-5d len=%-5d ck=%v\n",
					seq, ip.Src, udp.SrcPort, ip.Dst, udp.DstPort,
					udp.Length, checksumOK)
			}
		case capture.ProtoICMP:
			icmp, _, err := capture.ParseICMP(transport)
			if err == nil {
				fmt.Printf("[%d] ICMP %s -> %s type=%d code=%d ck=%v\n",
					seq, ip.Src, ip.Dst, icmp.Type, icmp.Code, checksumOK)
			}
		default:
			fmt.Printf("[%d] IPv4 %s -> %s proto=%d\n",
				seq, ip.Src, ip.Dst, ip.Protocol)
		}
	default:
		if eth.VLANID != 0 {
			fmt.Printf("[%d] VLAN id=%-4d type=0x%04x %s -> %s len=%d\n",
				seq, eth.VLANID, eth.EtherType, eth.Src, eth.Dst, len(raw))
		} else {
			fmt.Printf("[%d] ETH  type=0x%04x %s -> %s len=%d\n",
				seq, eth.EtherType, eth.Src, eth.Dst, len(raw))
		}
	}
}

func flagStr(flags uint8) string {
	m := []struct {
		bit  uint8
		name string
	}{
		{capture.FlagSYN, "SYN"},
		{capture.FlagACK, "ACK"},
		{capture.FlagFIN, "FIN"},
		{capture.FlagRST, "RST"},
		{capture.FlagPSH, "PSH"},
		{capture.FlagURG, "URG"},
	}
	result := ""
	for _, f := range m {
		if flags&f.bit != 0 {
			if result != "" {
				result += "|"
			}
			result += f.name
		}
	}
	if result == "" {
		return "NONE"
	}
	return result
}
```

## Common Mistakes

### Forgetting htons on the Protocol Argument

Wrong: `syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, syscall.ETH_P_ALL)` — this passes 0x0003 as a little-endian integer, but the kernel interprets it as network byte order (0x0300), which is not `ETH_P_ALL`.

What happens: the socket opens without error but receives only frames whose EtherType is 0x0300 (which is non-standard), so no packets arrive.

Fix: wrap the protocol with `htons`:
```go
int(htons(syscall.ETH_P_ALL))
```

### Aliasing Frame Bytes Into IP Address Fields

Wrong: `h.Src = net.IP(b[12:16])` — this stores a slice header into the frame buffer. When the buffer is reused for the next packet, `h.Src` changes silently.

What happens: code that reads `h.Src` after calling `Capture(buf)` again sees the source IP of the new packet instead of the one it stored.

Fix: copy the address bytes:
```go
h.Src = make(net.IP, 4)
copy(h.Src, b[12:16])
```

The parsers in this lesson already do this; the mistake is easy to reintroduce if you inline a parser for performance.

### Using `binary.LittleEndian` to Read Protocol Headers

Wrong: using `binary.LittleEndian.Uint16(b[12:14])` to read an EtherType — produces 0x0008 instead of 0x0800.

What happens: EtherType comparisons always fail on little-endian hardware (x86); the protocol switch falls through to a default case for every packet.

Fix: all multi-byte protocol fields are big-endian; always use `binary.BigEndian`.

### Verifying the Checksum With the Field Zeroed

Wrong: zeroing the checksum field before calling `VerifyIPv4Checksum` and then checking whether the result is zero.

What happens: you are computing the checksum, not verifying it. The result will be the correct checksum of the header, not a verification outcome.

Fix: call `VerifyIPv4Checksum` on the raw header as received, with the stored checksum field intact. The ones' complement identity means that summing all 16-bit words including the stored checksum produces 0xFFFF for any valid header.

### BPF Filter IP Protocol Offset Off by One

Wrong: loading the IP protocol byte from offset 14 (start of the IP header):
```go
{Code: 0x30, Jt: 0, Jf: 0, K: 0x0000000e}, // offset 14 = first byte of IP header (version+IHL)
```

What happens: the filter compares the version+IHL byte (0x45) to 6 (TCP), which is always false. No TCP frames are delivered.

Fix: the IP protocol field is at byte 9 of the IP header, which is at Ethernet offset 14+9 = 23 (0x17). Use `K: 0x00000017`.

## Verification

On Linux (with `CAP_NET_RAW` or root), run the full gate from `~/go-exercises/capture`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -count=1 -race ./...
go build ./...
```

The platform-independent tests (`frame_test.go`, `pcap_test.go`) pass on any OS without privileges. The `go build ./...` step compiles the Linux socket and BPF code only when `GOOS=linux`.

Grant `CAP_NET_RAW` without root to run the demo:

```bash
go build -o capture-demo ./cmd/demo
sudo setcap cap_net_raw+eip ./capture-demo
```

Display all packets on `eth0` (replace with your interface name):

```bash
./capture-demo -i eth0
```

Capture only TCP, save to pcap, open in Wireshark:

```bash
./capture-demo -i eth0 -tcp -w /tmp/tcp.pcap -n 100
wireshark /tmp/tcp.pcap
```

Compare frame counts with tcpdump to confirm no drops:

```bash
tcpdump -i eth0 -nn -c 100 tcp &
./capture-demo -i eth0 -tcp -n 100
```

Add one more test: `TestOnesComplementSumAllFF` — construct a 20-byte slice of `0xff` bytes, compute `onesComplementSum`, and assert the result equals `0xffff` (every word is `0xffff`; the carry-folded sum of N copies of `0xffff` converges to `0xffff`).

## Summary

- `AF_PACKET`/`SOCK_RAW` delivers complete Ethernet frames before demultiplexing; it requires `CAP_NET_RAW` and must be bound to an interface index.
- The protocol argument to `socket(2)` must be in network byte order via `htons`; forgetting this is the most common source of silent empty captures.
- All multi-byte protocol header fields are big-endian; use `encoding/binary.BigEndian` exclusively. Copy address fields out of the frame buffer before the buffer is reused.
- IPv4 checksum verification: sum all header words including the stored checksum field; a valid header produces 0xFFFF.
- Classic BPF filters run in the kernel before `recvfrom` returns, eliminating copies for non-matching frames. The IP protocol byte sits at Ethernet offset 23 (14 + 9), not 14.
- The pcap format uses little-endian encoding when the magic is `0xa1b2c3d4`; `incl_len` holds the truncated length while `orig_len` records the true on-wire size.
- Separate pure parsing code (testable on any OS) from Linux-only socket code (guarded by `//go:build linux`) so the correctness of the byte-level logic can be tested without kernel privileges.

## What's Next

Next: [Custom Network Protocol Stack](../09-custom-network-protocol-stack/09-custom-network-protocol-stack.md).

## Resources

- [Linux packet(7) man page](https://man7.org/linux/man-pages/man7/packet.7.html) — AF_PACKET socket semantics, PACKET_MR_PROMISC, SO_ATTACH_FILTER
- [Linux bpf(2) / filter documentation](https://www.kernel.org/doc/html/latest/networking/filter.html) — classic BPF instruction set, SockFprog, instruction encoding
- [RFC 791 — Internet Protocol](https://www.rfc-editor.org/rfc/rfc791) — IPv4 header format and checksum algorithm (section 3.1)
- [RFC 793 — Transmission Control Protocol](https://www.rfc-editor.org/rfc/rfc793) — TCP header format and pseudo-header checksum (section 3.1)
- [Wireshark pcap file format](https://wiki.wireshark.org/Development/LibpcapFileFormat) — global header, per-packet record layout, magic number variants
