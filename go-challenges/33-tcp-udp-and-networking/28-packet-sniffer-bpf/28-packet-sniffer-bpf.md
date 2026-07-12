# 28. Packet Sniffer with BPF

Raw packet capture operates below the socket layer: your program receives every
frame the NIC accepts, then picks what to keep. The hard parts are the
two-level filter design (a kernel-resident BPF program discards frames before
they ever reach userspace), hand-parsing multi-layer headers from big-endian
bytes, and the privilege boundary that makes testing a discipline problem as
much as a code problem. The live capture path requires cgo (libpcap); the
protocol dissectors and the pcap file writer are pure Go and are testable
offline with canned bytes.

```text
sniffer/
  go.mod
  frame.go           // Ethernet, IPv4, TCP, UDP header parsing (stdlib only)
  pcapwriter.go      // pcap file format writer (stdlib only)
  bpf.go             // BPF program assembly via golang.org/x/net/bpf
  capture.go         // live capture via gopacket/pcap  (//go:build cgo)
  frame_test.go      // hermetic tests + Example function
  cmd/demo/main.go   // runnable demo, no live interface required
```

## Concepts

### The Two-Level Architecture: Kernel Filter, Userspace Dissector

Every packet that arrives at the NIC triggers a kernel interrupt. Without
filtering, every packet is copied from kernel memory to userspace memory,
which means two memory allocations and a context switch per frame — easily
millions of system calls per second on a busy interface.

BPF (Berkeley Packet Filter, originally McCanne and Jacobson 1992) solves this
by running a tiny bytecode program *inside the kernel* before the copy. If the
program returns 0, the packet is dropped silently. If it returns N, up to N
bytes are copied to the ring buffer the userspace process polls. On a `tcpdump`
run filtering for one host out of a thousand, the BPF program rejects 999 out
of 1000 frames without ever crossing the kernel/user boundary.

libpcap (and `gopacket/pcap`) compiles filter expressions like `tcp port 80`
into classic BPF bytecode and attaches the program to the socket via
`setsockopt(SO_ATTACH_FILTER)`. eBPF is the modern successor (arbitrary
programs with maps, verified by the kernel), but pcap-style sniffers still
use classic BPF.

### Classic BPF: A Bytecode VM in the Kernel

Classic BPF is a Harvard-architecture RISC machine with two 32-bit registers
(accumulator A and index X), a 16-word scratch-memory store, and a
fixed-size program. Every instruction is four fields: `op`, `jt` (jump-if-true
offset), `jf` (jump-if-false offset), `k` (immediate). The program must end
with a `ret` — the kernel enforces this during attachment.

`golang.org/x/net/bpf` exposes typed Go structs for each BPF instruction and
an `Assemble` function that converts them to `[]bpf.RawInstruction`, which is
the format `pcap.Handle.SetBPFFilterBPF` (gopacket) accepts directly.

The critical instruction for variable-length IP headers is `LoadMemShift`:
it reads the low four bits of the byte at offset Off (the IHL field), multiplies
by four, and stores the result in X. After that, `LoadIndirect{Off: 16, Size: 2}`
reads two bytes at absolute position `X + 16` — which, for Ethernet + IPv4 with
any header length, is the TCP destination port.

### Protocol Layering and Network Byte Order

Ethernet wraps IP which wraps TCP or UDP which wraps the application. Each
layer prepends a fixed-format header. All header integer fields use network byte
order (big-endian); `encoding/binary.BigEndian` decodes them correctly.

```
 Offset  Field          Size  Notes
 ------  -----          ----  -----
 0       Dst MAC        6     Ethernet header
 6       Src MAC        6
 12      EtherType      2     0x0800 = IPv4, 0x86DD = IPv6
 14      Version|IHL    1     IP header; IHL*4 = header length
 23      Protocol       1     6 = TCP, 17 = UDP, 1 = ICMP
 26      Src IP         4
 30      Dst IP         4
 14+IHL*4  Src port     2     TCP/UDP start after variable-length IP header
 14+IHL*4+2  Dst port   2
```

The TCP data-offset field (high four bits of byte 12 within the TCP header,
in 32-bit words) encodes the TCP header length. A data-offset of 5 means a
20-byte header with no options.

### pcap File Format

A pcap file is a 24-byte global header followed by 16-byte per-packet records
each prepended to the raw frame bytes. The global header begins with a magic
number (0xA1B2C3D4 in native byte order) that lets tools auto-detect the
endianness. Every integer in the file is in the byte order the writer's machine
uses (native), which `binary.LittleEndian` produces correctly on x86/arm64.
Wireshark, `tcpdump -r`, and `tshark` all accept files written by the writer
in Exercise 2.

### Why Capture Requires Root

On Linux, opening a raw socket (`AF_PACKET, SOCK_RAW`) or calling
`pcap_open_live` requires `CAP_NET_RAW`. On macOS, `/dev/bpf*` devices are
owned by root. The conventional workaround for development is to grant
`CAP_NET_RAW` to a compiled binary with `setcap`, or to run tests that need
live capture behind a build tag (`//go:build integration`) and exclude them
from the default CI gate.

## Exercises

Set up the module:

```bash
go get golang.org/x/net@latest
go get github.com/google/gopacket@latest
```

The live-capture file (`capture.go`) compiles only when cgo is enabled. The
three stdlib-only files compile and test offline. The test gate does not touch
a network interface.

### Exercise 1: Ethernet, IPv4, TCP, and UDP Header Parsing

Create `frame.go`:

```go
package sniffer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// ErrShortPacket is returned when a packet is too short to contain a header.
var ErrShortPacket = errors.New("sniffer: packet too short")

// EtherType values carried in the Ethernet EtherType field.
const (
	EtherTypeIPv4 = 0x0800
	EtherTypeIPv6 = 0x86DD
	EtherTypeARP  = 0x0806
)

// EthernetHeader holds the decoded Ethernet II frame header.
type EthernetHeader struct {
	DstMAC    net.HardwareAddr
	SrcMAC    net.HardwareAddr
	EtherType uint16
}

// EthernetHeaderLen is the fixed length of an Ethernet II header in bytes.
const EthernetHeaderLen = 14

// ParseEthernet decodes the Ethernet header from the start of b.
// It returns the header and the payload starting immediately after the 14-byte
// header.
func ParseEthernet(b []byte) (EthernetHeader, []byte, error) {
	if len(b) < EthernetHeaderLen {
		return EthernetHeader{}, nil, fmt.Errorf("%w: need %d bytes, got %d",
			ErrShortPacket, EthernetHeaderLen, len(b))
	}
	h := EthernetHeader{
		DstMAC:    net.HardwareAddr(b[0:6]),
		SrcMAC:    net.HardwareAddr(b[6:12]),
		EtherType: binary.BigEndian.Uint16(b[12:14]),
	}
	return h, b[EthernetHeaderLen:], nil
}

// IPv4Header holds the decoded fields of an IPv4 header.
type IPv4Header struct {
	IHL      uint8 // actual header length in bytes (raw IHL field * 4)
	DSCP     uint8
	TotalLen uint16
	ID       uint16
	Flags    uint8
	FragOff  uint16
	TTL      uint8
	Protocol uint8
	Checksum uint16
	Src      net.IP
	Dst      net.IP
}

// IPv4MinHeaderLen is the minimum IPv4 header length (no options).
const IPv4MinHeaderLen = 20

// IP protocol numbers.
const (
	IPProtoICMP = 1
	IPProtoTCP  = 6
	IPProtoUDP  = 17
)

// ParseIPv4 decodes an IPv4 header from b. It returns the header and the
// payload that begins after the variable-length IP header (after the options,
// if any).
func ParseIPv4(b []byte) (IPv4Header, []byte, error) {
	if len(b) < IPv4MinHeaderLen {
		return IPv4Header{}, nil, fmt.Errorf("%w: IPv4 need %d bytes, got %d",
			ErrShortPacket, IPv4MinHeaderLen, len(b))
	}
	ihl := int((b[0] & 0x0F) * 4)
	if ihl < IPv4MinHeaderLen {
		return IPv4Header{}, nil, fmt.Errorf("sniffer: invalid IHL %d", ihl)
	}
	if ihl > len(b) {
		return IPv4Header{}, nil, fmt.Errorf("%w: IHL=%d exceeds buffer length %d",
			ErrShortPacket, ihl, len(b))
	}
	h := IPv4Header{
		IHL:      uint8(ihl),
		DSCP:     b[1] >> 2,
		TotalLen: binary.BigEndian.Uint16(b[2:4]),
		ID:       binary.BigEndian.Uint16(b[4:6]),
		Flags:    b[6] >> 5,
		FragOff:  binary.BigEndian.Uint16(b[6:8]) & 0x1FFF,
		TTL:      b[8],
		Protocol: b[9],
		Checksum: binary.BigEndian.Uint16(b[10:12]),
		Src:      net.IP(b[12:16]).To4(),
		Dst:      net.IP(b[16:20]).To4(),
	}
	return h, b[ihl:], nil
}

// TCPFlags packs the TCP control bits from byte 13 of the TCP header.
type TCPFlags uint8

const (
	FlagFIN = TCPFlags(1 << 0)
	FlagSYN = TCPFlags(1 << 1)
	FlagRST = TCPFlags(1 << 2)
	FlagPSH = TCPFlags(1 << 3)
	FlagACK = TCPFlags(1 << 4)
	FlagURG = TCPFlags(1 << 5)
)

// Has reports whether f contains the given flag bit.
func (f TCPFlags) Has(flag TCPFlags) bool { return f&flag != 0 }

// String returns a "|"-separated list of set flag names, or "none".
func (f TCPFlags) String() string {
	names := [...]struct {
		flag TCPFlags
		name string
	}{
		{FlagSYN, "SYN"},
		{FlagACK, "ACK"},
		{FlagFIN, "FIN"},
		{FlagRST, "RST"},
		{FlagPSH, "PSH"},
		{FlagURG, "URG"},
	}
	s := ""
	for _, n := range names {
		if f.Has(n.flag) {
			if s != "" {
				s += "|"
			}
			s += n.name
		}
	}
	if s == "" {
		return "none"
	}
	return s
}

// TCPHeader holds the decoded TCP header fields.
type TCPHeader struct {
	SrcPort    uint16
	DstPort    uint16
	Seq        uint32
	Ack        uint32
	DataOffset uint8 // TCP header length in bytes (raw data-offset field * 4)
	Flags      TCPFlags
	Window     uint16
	Checksum   uint16
	Urgent     uint16
}

// TCPMinHeaderLen is the minimum TCP header length (no options).
const TCPMinHeaderLen = 20

// ParseTCP decodes a TCP header from b. It returns the header and the payload
// starting after the variable-length TCP header.
func ParseTCP(b []byte) (TCPHeader, []byte, error) {
	if len(b) < TCPMinHeaderLen {
		return TCPHeader{}, nil, fmt.Errorf("%w: TCP need %d bytes, got %d",
			ErrShortPacket, TCPMinHeaderLen, len(b))
	}
	doff := int((b[12] >> 4) * 4)
	if doff < TCPMinHeaderLen {
		return TCPHeader{}, nil, fmt.Errorf("sniffer: invalid TCP data offset %d", doff)
	}
	if doff > len(b) {
		return TCPHeader{}, nil, fmt.Errorf("%w: TCP data offset %d exceeds buffer %d",
			ErrShortPacket, doff, len(b))
	}
	h := TCPHeader{
		SrcPort:    binary.BigEndian.Uint16(b[0:2]),
		DstPort:    binary.BigEndian.Uint16(b[2:4]),
		Seq:        binary.BigEndian.Uint32(b[4:8]),
		Ack:        binary.BigEndian.Uint32(b[8:12]),
		DataOffset: uint8(doff),
		Flags:      TCPFlags(b[13] & 0x3F),
		Window:     binary.BigEndian.Uint16(b[14:16]),
		Checksum:   binary.BigEndian.Uint16(b[16:18]),
		Urgent:     binary.BigEndian.Uint16(b[18:20]),
	}
	return h, b[doff:], nil
}

// UDPHeader holds the decoded UDP header fields.
type UDPHeader struct {
	SrcPort  uint16
	DstPort  uint16
	Length   uint16
	Checksum uint16
}

// UDPHeaderLen is the fixed UDP header length.
const UDPHeaderLen = 8

// ParseUDP decodes a UDP header from b. It returns the header and the payload.
func ParseUDP(b []byte) (UDPHeader, []byte, error) {
	if len(b) < UDPHeaderLen {
		return UDPHeader{}, nil, fmt.Errorf("%w: UDP need %d bytes, got %d",
			ErrShortPacket, UDPHeaderLen, len(b))
	}
	h := UDPHeader{
		SrcPort:  binary.BigEndian.Uint16(b[0:2]),
		DstPort:  binary.BigEndian.Uint16(b[2:4]),
		Length:   binary.BigEndian.Uint16(b[4:6]),
		Checksum: binary.BigEndian.Uint16(b[6:8]),
	}
	return h, b[UDPHeaderLen:], nil
}
```

`ParseIPv4` advances past the entire IP header including options (using the IHL
field), so `ParseTCP` or `ParseUDP` always receives bytes that start at the
transport layer, regardless of IP options.

### Exercise 2: pcap File Writer

Create `pcapwriter.go`:

```go
package sniffer

import (
	"encoding/binary"
	"io"
	"time"
)

// pcap global header constants.
// Magic 0xA1B2C3D4 written in native byte order signals the file's endianness
// to readers. Link type 1 is EN10MB (Ethernet).
const (
	pcapMagicNative    = 0xA1B2C3D4
	pcapVersionMajor   = 2
	pcapVersionMinor   = 4
	pcapSnapLen        = 65535
	pcapLinkTypeEN10MB = 1
)

// PCAPWriter writes raw frames in the libpcap file format. Files written by
// this type can be opened in Wireshark, tshark, and tcpdump -r without
// conversion.
type PCAPWriter struct {
	w io.Writer
}

// NewPCAPWriter writes the 24-byte pcap global header to w and returns a
// writer ready to accept packet records.
func NewPCAPWriter(w io.Writer) (*PCAPWriter, error) {
	// Global header layout (all fields in native byte order):
	//   [0:4]   magic number
	//   [4:6]   major version
	//   [6:8]   minor version
	//   [8:12]  thiszone  (GMT offset; 0 in practice)
	//   [12:16] sigfigs   (timestamp accuracy; 0 in practice)
	//   [16:20] snaplen
	//   [20:24] link type
	var hdr [24]byte
	binary.LittleEndian.PutUint32(hdr[0:4], pcapMagicNative)
	binary.LittleEndian.PutUint16(hdr[4:6], pcapVersionMajor)
	binary.LittleEndian.PutUint16(hdr[6:8], pcapVersionMinor)
	binary.LittleEndian.PutUint32(hdr[16:20], pcapSnapLen)
	binary.LittleEndian.PutUint32(hdr[20:24], pcapLinkTypeEN10MB)
	if _, err := w.Write(hdr[:]); err != nil {
		return nil, err
	}
	return &PCAPWriter{w: w}, nil
}

// WritePacket appends one packet record to the file. origLen is the original
// length of the packet on the wire (may exceed len(data) if it was truncated
// to snaplen). Pass len(data) for origLen when the full frame was captured.
func (pw *PCAPWriter) WritePacket(ts time.Time, origLen int, data []byte) error {
	// Per-packet record header (16 bytes, native byte order):
	//   [0:4]  ts_sec   (seconds since epoch)
	//   [4:8]  ts_usec  (microseconds)
	//   [8:12] incl_len (bytes present in data)
	//   [12:16] orig_len (original wire length)
	var rec [16]byte
	binary.LittleEndian.PutUint32(rec[0:4], uint32(ts.Unix()))
	binary.LittleEndian.PutUint32(rec[4:8], uint32(ts.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(rec[8:12], uint32(len(data)))
	binary.LittleEndian.PutUint32(rec[12:16], uint32(origLen))
	if _, err := pw.w.Write(rec[:]); err != nil {
		return err
	}
	_, err := pw.w.Write(data)
	return err
}
```

### Exercise 3: BPF Filter Assembly

Create `bpf.go` (requires `golang.org/x/net/bpf`):

```go
package sniffer

import (
	"golang.org/x/net/bpf"
)

// TCPPortFilter returns a slice of raw BPF instructions that accept Ethernet/
// IPv4 frames where the TCP source or destination port equals port. All other
// frames return 0 (drop). The assembled slice can be passed directly to
// (*pcap.Handle).SetBPFFilterBPF or to the VM in the bpf package for
// offline testing.
//
// Instruction layout and jump arithmetic (instructions indexed 0-10):
//
//	0  ldh  [12]            load EtherType
//	1  jneq 0x0800, +7     not IPv4 → drop at 9
//	2  ldb  [23]            load IP Protocol (ETH=14, IP.Protocol offset=9, total=23)
//	3  jneq 6, +5          not TCP → drop at 9
//	4  ldxb 4*([14]&0xf)   X = IHL*4 (variable IP header length)
//	5  ldh  [x+16]         TCP dst port = ETH(14) + X + 2 = X+16
//	6  jeq  port, +3       dst matches → accept at 10
//	7  ldh  [x+14]         TCP src port = ETH(14) + X + 0 = X+14
//	8  jeq  port, +1, +0   src matches → accept at 10; else fall through
//	9  ret  0              drop
//	10 ret  65535          accept (up to snaplen bytes)
func TCPPortFilter(port uint16) ([]bpf.RawInstruction, error) {
	insns := []bpf.Instruction{
		bpf.LoadAbsolute{Off: 12, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: 0x0800, SkipTrue: 7, SkipFalse: 0},
		bpf.LoadAbsolute{Off: 23, Size: 1},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: 6, SkipTrue: 5, SkipFalse: 0},
		bpf.LoadMemShift{Off: 14},
		bpf.LoadIndirect{Off: 16, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(port), SkipTrue: 3, SkipFalse: 0},
		bpf.LoadIndirect{Off: 14, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(port), SkipTrue: 1, SkipFalse: 0},
		bpf.RetConstant{Val: 0},
		bpf.RetConstant{Val: 65535},
	}
	return bpf.Assemble(insns)
}
```

The jump offsets encode "how many instructions to skip past the one immediately
after the jump." From instruction 1 the next instruction is 2; to reach the
drop at 9 requires skipping instructions 2 through 8, so `SkipTrue: 7`.

### Exercise 4: Live Capture with gopacket/pcap

Create `capture.go` (compiles only when cgo is available):

```go
//go:build cgo

package sniffer

import (
	"fmt"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

// CaptureConfig controls a live capture session.
type CaptureConfig struct {
	Interface string
	SnapLen   int32
	Promisc   bool
	Timeout   time.Duration
	Filter    string // BPF expression, e.g. "tcp port 80"
}

// CaptureSession wraps a live pcap handle and a packet source.
type CaptureSession struct {
	handle *pcap.Handle
	src    *gopacket.PacketSource
}

// OpenCapture opens a live capture on cfg.Interface, applies cfg.Filter as a
// BPF expression, and returns a session. The caller must call Close.
//
// Requires CAP_NET_RAW (Linux) or root access (macOS/BSD).
func OpenCapture(cfg CaptureConfig) (*CaptureSession, error) {
	if cfg.SnapLen == 0 {
		cfg.SnapLen = 65535
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = pcap.BlockForever
	}
	handle, err := pcap.OpenLive(cfg.Interface, cfg.SnapLen, cfg.Promisc, cfg.Timeout)
	if err != nil {
		return nil, fmt.Errorf("sniffer: open %s: %w", cfg.Interface, err)
	}
	if cfg.Filter != "" {
		if err := handle.SetBPFFilter(cfg.Filter); err != nil {
			handle.Close()
			return nil, fmt.Errorf("sniffer: set BPF filter %q: %w", cfg.Filter, err)
		}
	}
	src := gopacket.NewPacketSource(handle, handle.LinkType())
	return &CaptureSession{handle: handle, src: src}, nil
}

// Next returns the next captured packet. It blocks until a packet arrives or
// the capture timeout expires.
func (cs *CaptureSession) Next() (gopacket.Packet, error) {
	return cs.src.NextPacket()
}

// Stats returns packets received and dropped by the kernel since the session
// opened. Dropped packets indicate that the kernel ring buffer overflowed —
// either snaplen is too large or the BPF filter is too broad.
func (cs *CaptureSession) Stats() (recv, dropped uint, err error) {
	s, err := cs.handle.Stats()
	if err != nil {
		return 0, 0, err
	}
	return uint(s.PacketsReceived), uint(s.PacketsDropped), nil
}

// Close releases the pcap handle and its associated resources.
func (cs *CaptureSession) Close() {
	cs.handle.Close()
}
```

### Exercise 5: Tests and Demo

Create `frame_test.go` (all assertions use canned packet bytes; no network):

```go
package sniffer

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
)

// tcpSYNPacket is a hand-crafted 54-byte Ethernet/IPv4/TCP SYN frame.
// Layout: 14 bytes Ethernet + 20 bytes IPv4 (IHL=5) + 20 bytes TCP.
// Source 127.0.0.1:49192 → destination 127.0.0.1:80, SEQ=1, flags=SYN.
var tcpSYNPacket = []byte{
	// Ethernet header
	0x00, 0x0c, 0x29, 0xab, 0xcd, 0xef, // dst MAC
	0x00, 0x50, 0x56, 0x12, 0x34, 0x56, // src MAC
	0x08, 0x00, // EtherType = IPv4
	// IPv4 header (20 bytes, IHL=5)
	0x45, 0x00, 0x00, 0x28, // version/IHL=0x45, DSCP=0, total length=40
	0xab, 0xcd, 0x40, 0x00, // ID, Flags=DF, fragment offset=0
	0x40, 0x06, 0x00, 0x00, // TTL=64, protocol=6 (TCP), checksum=0
	0x7f, 0x00, 0x00, 0x01, // src IP = 127.0.0.1
	0x7f, 0x00, 0x00, 0x01, // dst IP = 127.0.0.1
	// TCP header (20 bytes, data offset=5)
	0xc0, 0x28, // src port = 49192
	0x00, 0x50, // dst port = 80
	0x00, 0x00, 0x00, 0x01, // seq = 1
	0x00, 0x00, 0x00, 0x00, // ack = 0
	0x50, 0x02, // data offset=5 (20 bytes), flags=0x02 (SYN)
	0xff, 0xff, // window = 65535
	0x00, 0x00, // checksum = 0
	0x00, 0x00, // urgent pointer = 0
}

// udpDNSPacket is a hand-crafted 42-byte Ethernet/IPv4/UDP frame.
// Layout: 14 bytes Ethernet + 20 bytes IPv4 + 8 bytes UDP (no payload).
// Source 192.168.1.1:32848 → destination 8.8.8.8:53.
var udpDNSPacket = []byte{
	// Ethernet header
	0x00, 0x0c, 0x29, 0xab, 0xcd, 0xef,
	0x00, 0x50, 0x56, 0x12, 0x34, 0x56,
	0x08, 0x00,
	// IPv4 header (20 bytes)
	0x45, 0x00, 0x00, 0x1c, // total length=28 (20 IP + 8 UDP)
	0x12, 0x34, 0x00, 0x00,
	0x40, 0x11, 0x00, 0x00, // TTL=64, protocol=17 (UDP), checksum=0
	0xc0, 0xa8, 0x01, 0x01, // src IP = 192.168.1.1
	0x08, 0x08, 0x08, 0x08, // dst IP = 8.8.8.8
	// UDP header (8 bytes)
	0x80, 0x50, // src port = 32848
	0x00, 0x35, // dst port = 53
	0x00, 0x08, // length = 8 (header only, no payload)
	0x00, 0x00, // checksum = 0
}

func TestParseEthernetTCPSYN(t *testing.T) {
	t.Parallel()

	eth, payload, err := ParseEthernet(tcpSYNPacket)
	if err != nil {
		t.Fatalf("ParseEthernet: %v", err)
	}
	if eth.EtherType != EtherTypeIPv4 {
		t.Errorf("EtherType = 0x%04x, want 0x%04x", eth.EtherType, EtherTypeIPv4)
	}
	if len(payload) != 40 { // 20 IP + 20 TCP
		t.Errorf("payload len = %d, want 40", len(payload))
	}
}

func TestParseIPv4TCPSYN(t *testing.T) {
	t.Parallel()

	_, payload, _ := ParseEthernet(tcpSYNPacket)
	ip, tcpPayload, err := ParseIPv4(payload)
	if err != nil {
		t.Fatalf("ParseIPv4: %v", err)
	}
	if ip.Protocol != IPProtoTCP {
		t.Errorf("Protocol = %d, want %d (TCP)", ip.Protocol, IPProtoTCP)
	}
	if ip.TTL != 64 {
		t.Errorf("TTL = %d, want 64", ip.TTL)
	}
	if !ip.Src.Equal(ip.Dst) {
		t.Errorf("src %v != dst %v, both should be 127.0.0.1", ip.Src, ip.Dst)
	}
	// IHL=5 (20 bytes) consumed; remaining is the 20-byte TCP header.
	if len(tcpPayload) != 20 {
		t.Errorf("tcpPayload len = %d, want 20", len(tcpPayload))
	}
}

func TestParseTCPSYN(t *testing.T) {
	t.Parallel()

	_, ethPayload, _ := ParseEthernet(tcpSYNPacket)
	_, tcpPayload, _ := ParseIPv4(ethPayload)
	tcp, payload, err := ParseTCP(tcpPayload)
	if err != nil {
		t.Fatalf("ParseTCP: %v", err)
	}
	if tcp.DstPort != 80 {
		t.Errorf("DstPort = %d, want 80", tcp.DstPort)
	}
	if tcp.SrcPort != 49192 {
		t.Errorf("SrcPort = %d, want 49192", tcp.SrcPort)
	}
	if tcp.Seq != 1 {
		t.Errorf("Seq = %d, want 1", tcp.Seq)
	}
	if !tcp.Flags.Has(FlagSYN) {
		t.Errorf("Flags = %s, want FlagSYN set", tcp.Flags)
	}
	if tcp.Flags.Has(FlagACK) {
		t.Errorf("Flags = %s, want FlagACK unset", tcp.Flags)
	}
	if len(payload) != 0 {
		t.Errorf("TCP payload len = %d, want 0 (no data in SYN)", len(payload))
	}
}

func TestParseUDPDNS(t *testing.T) {
	t.Parallel()

	_, ethPayload, _ := ParseEthernet(udpDNSPacket)
	ip, udpPayload, err := ParseIPv4(ethPayload)
	if err != nil {
		t.Fatalf("ParseIPv4: %v", err)
	}
	if ip.Protocol != IPProtoUDP {
		t.Errorf("Protocol = %d, want %d (UDP)", ip.Protocol, IPProtoUDP)
	}
	udp, _, err := ParseUDP(udpPayload)
	if err != nil {
		t.Fatalf("ParseUDP: %v", err)
	}
	if udp.DstPort != 53 {
		t.Errorf("DstPort = %d, want 53 (DNS)", udp.DstPort)
	}
	if udp.SrcPort != 32848 {
		t.Errorf("SrcPort = %d, want 32848", udp.SrcPort)
	}
}

func TestParseEthernetTooShort(t *testing.T) {
	t.Parallel()

	_, _, err := ParseEthernet([]byte{0x00, 0x01, 0x02})
	if !errors.Is(err, ErrShortPacket) {
		t.Fatalf("err = %v, want ErrShortPacket", err)
	}
}

func TestParseIPv4TooShort(t *testing.T) {
	t.Parallel()

	_, _, err := ParseIPv4([]byte{0x45, 0x00})
	if !errors.Is(err, ErrShortPacket) {
		t.Fatalf("err = %v, want ErrShortPacket", err)
	}
}

func TestParseTCPTooShort(t *testing.T) {
	t.Parallel()

	_, _, err := ParseTCP([]byte{0x00, 0x50})
	if !errors.Is(err, ErrShortPacket) {
		t.Fatalf("err = %v, want ErrShortPacket", err)
	}
}

func TestTCPFlagsString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		flags TCPFlags
		want  string
	}{
		{FlagSYN, "SYN"},
		{FlagSYN | FlagACK, "SYN|ACK"},
		{FlagFIN | FlagACK, "ACK|FIN"},
		{TCPFlags(0), "none"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.flags.String(); got != tc.want {
				t.Errorf("TCPFlags(0x%02x).String() = %q, want %q",
					uint8(tc.flags), got, tc.want)
			}
		})
	}
}

func TestPCAPWriterRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	pw, err := NewPCAPWriter(&buf)
	if err != nil {
		t.Fatalf("NewPCAPWriter: %v", err)
	}
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := pw.WritePacket(ts, len(tcpSYNPacket), tcpSYNPacket); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	data := buf.Bytes()
	// 24-byte global header + 16-byte per-packet record header + 54 bytes of frame.
	wantLen := 24 + 16 + len(tcpSYNPacket)
	if len(data) != wantLen {
		t.Fatalf("pcap output len = %d, want %d", len(data), wantLen)
	}
	// Global header bytes [0:4] are the magic number in native (little-endian) order.
	magic := uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24
	if magic != pcapMagicNative {
		t.Errorf("magic = 0x%08x, want 0x%08x", magic, pcapMagicNative)
	}
	// The frame data must be preserved byte-for-byte starting at offset 40.
	if !bytes.Equal(data[40:], tcpSYNPacket) {
		t.Error("frame bytes in pcap file do not match original packet")
	}
}

// ExampleParseTCP demonstrates chained header parsing on the loopback SYN frame.
func ExampleParseTCP() {
	_, ethPayload, _ := ParseEthernet(tcpSYNPacket)
	_, tcpPayload, _ := ParseIPv4(ethPayload)
	tcp, _, _ := ParseTCP(tcpPayload)
	fmt.Printf("src=%d dst=%d flags=%s\n", tcp.SrcPort, tcp.DstPort, tcp.Flags)
	// Output: src=49192 dst=80 flags=SYN
}
```

Your turn: add `TestParseIPv4OptionsFrame` that constructs a 24-byte IPv4 header
(IHL=6, one 4-byte option word) prepended to a minimal UDP header, calls
`ParseIPv4`, and asserts that the returned IHL equals 24 and the UDP payload
starts at the correct offset.

Create `cmd/demo/main.go`:

```go
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"example.com/sniffer"
)

func main() {
	raw := flag.Bool("raw", false, "parse the built-in test frame instead of reading a file")
	flag.Parse()

	if *raw {
		demoFrame()
		return
	}

	fmt.Fprintln(os.Stderr, "usage: go run ./cmd/demo -raw")
	fmt.Fprintln(os.Stderr, "       (live capture: sudo go run ./cmd/demo -iface eth0 requires cgo build)")
	os.Exit(1)
}

func demoFrame() {
	// 54-byte Ethernet/IPv4/TCP SYN frame, loopback 127.0.0.1:49192 -> :80.
	frame := []byte{
		0x00, 0x0c, 0x29, 0xab, 0xcd, 0xef,
		0x00, 0x50, 0x56, 0x12, 0x34, 0x56,
		0x08, 0x00,
		0x45, 0x00, 0x00, 0x28,
		0xab, 0xcd, 0x40, 0x00,
		0x40, 0x06, 0x00, 0x00,
		0x7f, 0x00, 0x00, 0x01,
		0x7f, 0x00, 0x00, 0x01,
		0xc0, 0x28, 0x00, 0x50,
		0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00,
		0x50, 0x02, 0xff, 0xff,
		0x00, 0x00, 0x00, 0x00,
	}

	eth, ethPayload, err := sniffer.ParseEthernet(frame)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stdout, "Ethernet: dst=%s src=%s type=0x%04x\n",
		eth.DstMAC, eth.SrcMAC, eth.EtherType)

	ip, tcpPayload, err := sniffer.ParseIPv4(ethPayload)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stdout, "IPv4:     src=%s dst=%s ttl=%d proto=%d ihl=%d\n",
		ip.Src, ip.Dst, ip.TTL, ip.Protocol, ip.IHL)

	tcp, _, err := sniffer.ParseTCP(tcpPayload)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stdout, "TCP:      src=%d dst=%d seq=%d flags=%s window=%d\n",
		tcp.SrcPort, tcp.DstPort, tcp.Seq, tcp.Flags, tcp.Window)
}
```

Run the demo without root:

```bash
go run ./cmd/demo -raw
```

Expected output:

```
Ethernet: dst=00:0c:29:ab:cd:ef src=00:50:56:12:34:56 type=0x0800
IPv4:     src=127.0.0.1 dst=127.0.0.1 ttl=64 proto=6 ihl=20
TCP:      src=49192 dst=80 seq=1 flags=SYN window=65535
```

## Common Mistakes

### Treating the IP Header Length as Fixed at 20 Bytes

Wrong: reading TCP from `packet[34:]` (ETH 14 + IP 20 = 34). If the IP header
carries options (IHL > 5, common in traceroute and timestamps), the TCP header
starts at `14 + IHL*4`, not at 34. The parser silently reads option bytes as
TCP fields and produces garbage port numbers.

Fix: always use the IHL field. `ParseIPv4` returns the transport payload
starting at the correct offset; `ParseTCP` reads from that returned slice, not
from a hard-coded offset.

### Forgetting Network Byte Order on Multi-Byte Fields

Wrong: `port := uint16(b[0]) | uint16(b[1])<<8` — this is little-endian. TCP
and IP integers are big-endian (network byte order). The source and destination
ports, the sequence number, the total length — all will be byte-swapped.

Fix: `port := binary.BigEndian.Uint16(b[0:2])`. All multi-byte header fields
use `binary.BigEndian`. On little-endian hardware the difference is invisible
until a test fails or Wireshark shows the "wrong" port.

### Miscounting BPF Jump Offsets

Wrong: computing the jump offset as the absolute target instruction index. BPF
jump offsets are *relative*: `SkipTrue: N` skips `N` instructions past the
one immediately following the jump. Off-by-one here means silently accepting
frames that should be dropped or dropping ones that should pass.

Fix: count forward from the instruction after the jump to the target. Document
the jump table explicitly (as the comment above `TCPPortFilter` does) before
writing a single number.

### Running Network Tests in the Default Test Path

Wrong: a test that calls `pcap.OpenLive` without a build tag. It fails in CI
(no root), on any machine without libpcap installed, and when cgo is disabled.

Fix: gate everything that touches a live interface behind `//go:build integration`
and run `go test -tags integration ./...` only in environments that have the
right privileges. The default `go test ./...` must be hermetic.

### Closing the pcap Handle Before Draining the Packet Source

Wrong: calling `cs.Close()` immediately after reading the last packet. The
`PacketSource` may still hold buffered packets or be mid-decode. The handle
is freed while the source still references it.

Fix: drain the channel or stop reading first, then call `cs.Close()`. For
controlled teardown, cancel the context passed to the packet loop and let the
loop exit naturally before closing.

## Verification

The stdlib-only files (frame.go, pcapwriter.go, frame_test.go) pass gofmt and
go vet without any external dependencies. The full build (bpf.go, capture.go)
requires `golang.org/x/net` and `github.com/google/gopacket` (libpcap).

From `~/go-exercises/sniffer`:

```bash
# Format check — must print nothing
test -z "$(gofmt -l .)"

# Vet — must print nothing
go vet ./...

# Build everything that does not need cgo (capture.go is excluded automatically)
CGO_ENABLED=0 go build example.com/sniffer

# Run offline tests (no network, no root, no cgo required)
go test -count=1 -race ./...

# Run the demo against the built-in test frame
go run ./cmd/demo -raw

# Build and test the full module (requires libpcap-dev and cgo)
# CGO_ENABLED=1 go test -count=1 -race ./...
```

`go test -count=1 -race ./...` is the verification. There is no program to
eyeball. `ExampleParseTCP` in `frame_test.go` is auto-verified by `go test`;
if the output changes, the test fails.

## Summary

- BPF runs a bytecode program in the kernel before copying frames to userspace;
  `golang.org/x/net/bpf` assembles typed Go structs into raw BPF instructions.
- `LoadMemShift{Off: 14}` extracts the variable IP header length (IHL*4) into
  the X register so subsequent `LoadIndirect` instructions find the TCP ports at
  the right offset regardless of IP options.
- All multi-byte header fields are big-endian (network byte order); decode with
  `encoding/binary.BigEndian`.
- A pcap file is a 24-byte global header plus 16-byte per-packet records; the
  magic number encodes endianness. Files written by `PCAPWriter` open in
  Wireshark without conversion.
- Live capture (`gopacket/pcap`) requires cgo and elevated privileges; isolate
  it behind `//go:build cgo` and `//go:build integration` so the default test
  gate is always hermetic.
- The dissector layer (frame.go, pcapwriter.go) is pure Go and is fully testable
  offline with hand-crafted packet bytes.

## What's Next

Next: [GMP Model](../../34-runtime-scheduler/01-gmp-model/01-gmp-model.md).

## Resources

- [golang.org/x/net/bpf package](https://pkg.go.dev/golang.org/x/net/bpf) — Go BPF assembler and classic BPF instruction set reference
- [RFC 791: Internet Protocol (IPv4)](https://datatracker.ietf.org/doc/html/rfc791) — authoritative IPv4 header field definitions
- [RFC 9293: Transmission Control Protocol](https://datatracker.ietf.org/doc/html/rfc9293) — authoritative TCP header and flag definitions (updates RFC 793)
- [pcap File Format — Wireshark Wiki](https://wiki.wireshark.org/Development/LibpcapFileFormat) — pcap global header and per-packet record layout
- [The BSD Packet Filter: A New Architecture for User-level Packet Capture (McCanne & Jacobson, 1993)](https://www.tcpdump.org/papers/bpf-usenix93.pdf) — original BPF design paper, explains the two-level kernel/user architecture
