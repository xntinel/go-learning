# 3. Netlink Socket Interface

Linux exposes network configuration -- interfaces, addresses, routing tables -- through **Netlink**, a kernel-userspace IPC mechanism built on `AF_NETLINK` sockets. Netlink is the protocol that `ip link`, `ip addr`, and `ip route` use internally. Learning it at the binary level reveals exactly what happens when you run those commands and gives you the ability to monitor network events in real time without polling, add and remove addresses programmatically, or build a network management tool in pure Go.

The hard parts are the wire format (16-byte headers, 4-byte alignment, nested TLV attributes), multi-part responses (the `NLM_F_MULTI` / `NLMSG_DONE` handshake), and error handling (the kernel returns errno values embedded in `NLMSG_ERROR` responses). Go's `syscall` package exposes the raw types and `ParseNetlinkMessage` to decode the binary stream; everything else you build yourself.

```text
netlinkmon/
  go.mod
  netlink.go        # Conn, Open, sendDump, recvMessages, appendAttr, parseAttrs
  link.go           # Link type, ListLinks
  addr.go           # Addr type, ListAddrs
  route.go          # Route type, ListRoutes
  monitor.go        # EventKind, Event, MonitorConn, OpenMonitor, Monitor
  netlink_test.go   # unit and integration tests (linux)
  cmd/demo/main.go  # runnable demo
```

All files carry `//go:build linux` because Netlink is a Linux-only facility. The only dependency is Go's `syscall` stdlib; no external modules are required.

## Concepts

### The Netlink Socket Model

A Netlink socket is created with `AF_NETLINK` as the domain and a protocol number that selects the kernel subsystem. For network configuration the protocol is `NETLINK_ROUTE`. Once bound, the socket operates in two modes:

- **Request / response**: send a `RTM_GET*` request with `NLM_F_REQUEST | NLM_F_DUMP`, read response messages until `NLMSG_DONE` terminates the sequence.
- **Multicast subscription**: bind with a `Groups` bitmask in `SockaddrNetlink`; the kernel pushes unsolicited notifications when subscribed events occur (interface state change, address added, route changed).

The socket is `SOCK_RAW`. The caller constructs and parses the binary message layout directly; there is no higher-level framing protocol.

### Wire Format: nlmsghdr, 4-Byte Alignment, TLV Attributes

Every Netlink message starts with a 16-byte header (`syscall.NlMsghdr`):

```text
Offset  Size  Field
     0     4  Len    total message length including header
     4     2  Type   RTM_GETLINK, NLMSG_DONE, NLMSG_ERROR, ...
     6     2  Flags  NLM_F_REQUEST | NLM_F_DUMP | ...
     8     4  Seq    sequence number (matches request to response)
    12     4  Pid    sender PID (0 on send; kernel fills it in)
```

After the header comes a protocol-specific body (`ifinfomsg`, `ifaddrmsg`, `rtmsg`, ...) followed by zero or more type-length-value attributes (`rtattr`). All lengths and start offsets of messages and attributes are rounded up to 4-byte boundaries:

```text
NLMSG_ALIGN(n) = (n + 3) & ~3
```

In Go this is `(n + 3) &^ 3` (the `&^` operator is bitwise AND NOT). Forgetting alignment is the most common source of malformed messages.

Attributes can be nested: an attribute whose `Type` field has bit 15 set (`NLA_F_NESTED`) contains more attributes in its value payload rather than raw data.

### Multi-Part Responses and Error Replies

A dump request returns multiple response messages, each with `NLM_F_MULTI` set. The sequence ends when the kernel sends a message with `Type == NLMSG_DONE`. A correct receive loop reads until `NLMSG_DONE`, not until `Recvfrom` returns zero bytes (it never does on a blocking socket).

Error replies have `Type == NLMSG_ERROR`. The first four bytes of the data payload are a signed `int32` errno; negative values are errors, zero is an acknowledgment (ACK). Failing to check for `NLMSG_ERROR` before interpreting the payload causes silent data corruption.

### RTNETLINK: Message Types, Bodies, and Attributes

The `NETLINK_ROUTE` protocol uses three message-type groups:

| Action    | Links        | Addresses   | Routes       |
|-----------|--------------|-------------|--------------|
| Dump      | RTM_GETLINK  | RTM_GETADDR | RTM_GETROUTE |
| Add       | RTM_NEWLINK  | RTM_NEWADDR | RTM_NEWROUTE |
| Delete    | RTM_DELLINK  | RTM_DELADDR | RTM_DELROUTE |
| Event     | RTM_NEWLINK  | RTM_NEWADDR | RTM_NEWROUTE |
|           | RTM_DELLINK  | RTM_DELADDR | RTM_DELROUTE |

The message body that follows the header depends on the type:

- **Link messages** (`syscall.SizeofIfInfomsg` = 16 bytes): Family, Pad, Type (ARPHRD_*), Index (int32), Flags (IFF_*), Change. Attributes: `IFLA_IFNAME` (NUL-terminated name), `IFLA_ADDRESS` (MAC bytes), `IFLA_MTU` (uint32), `IFLA_OPERSTATE` (uint8).
- **Address messages** (`syscall.SizeofIfAddrmsg` = 8 bytes): Family, Prefixlen, Flags, Scope, Index (uint32). Attributes: `IFA_ADDRESS` (IP bytes), `IFA_LOCAL`.
- **Route messages** (12 bytes): Family, DstLen, SrcLen, Tos, Table, Protocol, Scope, Type, Flags (uint32). Attributes: `RTA_DST`, `RTA_GATEWAY`, `RTA_OIF` (output interface index, uint32), `RTA_PRIORITY` (uint32).

All multi-byte integers in Netlink messages are in **host byte order** (little-endian on x86/arm64). Use `encoding/binary.NativeEndian`, not `binary.BigEndian`.

### Multicast Groups and the Event Monitor

To subscribe to network events, set the `Groups` field of `SockaddrNetlink` to a bitmask where bit N-1 is set for group number N:

```text
Groups = (1 << (RTNLGRP_LINK - 1)) | (1 << (RTNLGRP_IPV4_IFADDR - 1)) | ...
```

The kernel pushes unsolicited messages to the socket whenever matching events occur. A background goroutine reads these messages in a blocking `Recvfrom` loop and forwards decoded `Event` values on a channel. To cancel, call `Close` on the monitoring connection: the blocked `Recvfrom` returns immediately with `EBADF`, and the goroutine exits. A `sync.Once` guards the close so the fd is released exactly once whether cancellation or explicit close happens first.

## Exercises

All files carry `//go:build linux`.

### Exercise 1: Core Connection — netlink.go

This file owns socket lifecycle, message framing, and the shared helpers used by the rest of the package.

Create `netlink.go`:

```go
//go:build linux

package netlinkmon

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"syscall"
)

// nlAlign rounds n up to the next 4-byte Netlink boundary (NLMSG_ALIGN).
func nlAlign(n int) int {
	return (n + 3) &^ 3
}

// Conn is a bound NETLINK_ROUTE socket used for request/response operations.
type Conn struct {
	fd  int
	seq uint32 // accessed atomically
}

// Open creates and binds an AF_NETLINK/NETLINK_ROUTE socket.
func Open() (*Conn, error) {
	fd, err := syscall.Socket(
		syscall.AF_NETLINK,
		syscall.SOCK_RAW|syscall.SOCK_CLOEXEC,
		syscall.NETLINK_ROUTE,
	)
	if err != nil {
		return nil, fmt.Errorf("netlinkmon: socket: %w", err)
	}
	lsa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(fd, lsa); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("netlinkmon: bind: %w", err)
	}
	return &Conn{fd: fd}, nil
}

// Close releases the underlying socket descriptor.
func (c *Conn) Close() error {
	if err := syscall.Close(c.fd); err != nil {
		return fmt.Errorf("netlinkmon: close: %w", err)
	}
	return nil
}

// sendDump sends an RTM_GET* dump request for the given message type and
// address family (AF_UNSPEC = 0 requests all families).
//
// Wire layout: nlmsghdr (16 B) + rtgenmsg.Family (1 B) + 3 B padding = 20 B.
func (c *Conn) sendDump(msgType uint16, family uint8) error {
	const bodyLen = 1 // rtgenmsg contains one byte: Family
	msgLen := syscall.SizeofNlMsghdr + bodyLen
	buf := make([]byte, nlAlign(msgLen))

	seq := atomic.AddUint32(&c.seq, 1)
	binary.NativeEndian.PutUint32(buf[0:], uint32(nlAlign(msgLen)))
	binary.NativeEndian.PutUint16(buf[4:], msgType)
	binary.NativeEndian.PutUint16(buf[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(buf[8:], seq)
	binary.NativeEndian.PutUint32(buf[12:], 0) // PID: kernel fills this in
	buf[syscall.SizeofNlMsghdr] = family       // rtgenmsg.Family

	peer := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	return syscall.Sendto(c.fd, buf, 0, peer)
}

// recvMessages reads all response messages for a dump request, accumulating
// until the kernel sends NLMSG_DONE (end-of-multipart marker).
func (c *Conn) recvMessages() ([]syscall.NetlinkMessage, error) {
	var out []syscall.NetlinkMessage
	buf := make([]byte, 65536)
	for {
		n, _, err := syscall.Recvfrom(c.fd, buf, 0)
		if err != nil {
			return nil, fmt.Errorf("netlinkmon: recvfrom: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, fmt.Errorf("netlinkmon: parse: %w", err)
		}
		done := false
		for _, m := range msgs {
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				done = true
			case syscall.NLMSG_ERROR:
				if len(m.Data) < 4 {
					return nil, fmt.Errorf("netlinkmon: truncated NLMSG_ERROR")
				}
				// errno is negative for errors; zero signals an ACK.
				errno := int32(binary.NativeEndian.Uint32(m.Data[:4]))
				if errno != 0 {
					return nil, fmt.Errorf("netlinkmon: kernel error: %w", syscall.Errno(-errno))
				}
			default:
				out = append(out, m)
			}
		}
		if done {
			return out, nil
		}
	}
}

// sendModify sends a single RTM_NEW* or RTM_DEL* modification request with
// NLM_F_ACK and waits for the kernel ACK or error reply.
func (c *Conn) sendModify(msgType uint16, flags uint16, body []byte) error {
	msgLen := syscall.SizeofNlMsghdr + len(body)
	buf := make([]byte, nlAlign(msgLen))

	seq := atomic.AddUint32(&c.seq, 1)
	binary.NativeEndian.PutUint32(buf[0:], uint32(nlAlign(msgLen)))
	binary.NativeEndian.PutUint16(buf[4:], msgType)
	binary.NativeEndian.PutUint16(buf[6:], flags|syscall.NLM_F_REQUEST|syscall.NLM_F_ACK)
	binary.NativeEndian.PutUint32(buf[8:], seq)
	binary.NativeEndian.PutUint32(buf[12:], 0)
	copy(buf[syscall.SizeofNlMsghdr:], body)

	peer := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Sendto(c.fd, buf, 0, peer); err != nil {
		return fmt.Errorf("netlinkmon: send: %w", err)
	}
	// recvMessages reads until NLMSG_DONE or an error ACK.
	_, err := c.recvMessages()
	return err
}

// appendAttr appends a Netlink TLV attribute to buf and returns the new slice.
// The attribute payload is padded to the next 4-byte boundary.
func appendAttr(buf []byte, attrType uint16, data []byte) []byte {
	const hdrLen = 4 // sizeof(rtattr): Len uint16 + Type uint16
	total := nlAlign(hdrLen + len(data))
	a := make([]byte, total)
	binary.NativeEndian.PutUint16(a[0:], uint16(hdrLen+len(data)))
	binary.NativeEndian.PutUint16(a[2:], attrType)
	copy(a[hdrLen:], data)
	return append(buf, a...)
}

// parseAttrs parses a TLV attribute chain from b.
// b must already have the message-type-specific fixed header stripped.
func parseAttrs(b []byte) []syscall.NetlinkRouteAttr {
	var attrs []syscall.NetlinkRouteAttr
	for len(b) >= 4 {
		attrLen := int(binary.NativeEndian.Uint16(b[0:]))
		attrType := binary.NativeEndian.Uint16(b[2:])
		if attrLen < 4 || attrLen > len(b) {
			break
		}
		attrs = append(attrs, syscall.NetlinkRouteAttr{
			Attr:  syscall.RtAttr{Len: uint16(attrLen), Type: attrType},
			Value: b[4:attrLen],
		})
		b = b[nlAlign(attrLen):]
	}
	return attrs
}
```

`sendDump` encodes header fields with `binary.NativeEndian` instead of casting an unsafe pointer, which avoids alignment faults on architectures that require it. `recvMessages` terminates on `NLMSG_DONE`, not on a zero-byte read. `parseAttrs` is a local replacement for `syscall.ParseNetlinkRouteAttr` that explicitly strips a caller-specified number of header bytes, making the offset arithmetic visible and testable.

### Exercise 2: Link Discovery — link.go

Create `link.go`:

```go
//go:build linux

package netlinkmon

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
)

// Link describes one network interface as reported by the kernel.
type Link struct {
	Index        int
	Name         string
	HardwareAddr net.HardwareAddr
	MTU          int
	Flags        net.Flags // IFF_UP, IFF_LOOPBACK, etc. mapped to net.Flag*
	OperState    uint8     // IF_OPER_* kernel constant (0=unknown, 6=up)
}

// IsUp reports whether the interface has the IFF_UP flag set.
func (l Link) IsUp() bool {
	return l.Flags&net.FlagUp != 0
}

// ListLinks sends RTM_GETLINK and returns all network interfaces.
func (c *Conn) ListLinks() ([]Link, error) {
	if err := c.sendDump(syscall.RTM_GETLINK, syscall.AF_UNSPEC); err != nil {
		return nil, fmt.Errorf("ListLinks: %w", err)
	}
	msgs, err := c.recvMessages()
	if err != nil {
		return nil, fmt.Errorf("ListLinks: %w", err)
	}
	links := make([]Link, 0, len(msgs))
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWLINK {
			continue
		}
		l, err := parseLink(m)
		if err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, nil
}

func parseLink(m syscall.NetlinkMessage) (Link, error) {
	if len(m.Data) < syscall.SizeofIfInfomsg {
		return Link{}, fmt.Errorf("parseLink: short ifinfomsg (%d bytes)", len(m.Data))
	}
	// ifinfomsg layout (16 bytes):
	//   [0]   Family uint8
	//   [1]   Pad    uint8
	//   [2:4] Type   uint16  (ARPHRD_*)
	//   [4:8] Index  int32
	//   [8:12] Flags uint32  (IFF_*)
	//   [12:16] Change uint32
	index := int(int32(binary.NativeEndian.Uint32(m.Data[4:])))
	rawFlags := binary.NativeEndian.Uint32(m.Data[8:])
	l := Link{
		Index: index,
		Flags: kernelFlagsToNet(rawFlags),
	}
	for _, a := range parseAttrs(m.Data[syscall.SizeofIfInfomsg:]) {
		switch a.Attr.Type {
		case syscall.IFLA_IFNAME:
			// The kernel sends a NUL-terminated C string.
			name := a.Value
			if len(name) > 0 && name[len(name)-1] == 0 {
				name = name[:len(name)-1]
			}
			l.Name = string(name)
		case syscall.IFLA_ADDRESS:
			l.HardwareAddr = net.HardwareAddr(append([]byte(nil), a.Value...))
		case syscall.IFLA_MTU:
			if len(a.Value) == 4 {
				l.MTU = int(binary.NativeEndian.Uint32(a.Value))
			}
		case syscall.IFLA_OPERSTATE:
			if len(a.Value) == 1 {
				l.OperState = a.Value[0]
			}
		}
	}
	return l, nil
}

// kernelFlagsToNet maps the IFF_* bitmask received from the kernel to
// the net.Flag* constants used by the standard library.
func kernelFlagsToNet(raw uint32) net.Flags {
	var f net.Flags
	if raw&syscall.IFF_UP != 0 {
		f |= net.FlagUp
	}
	if raw&syscall.IFF_BROADCAST != 0 {
		f |= net.FlagBroadcast
	}
	if raw&syscall.IFF_LOOPBACK != 0 {
		f |= net.FlagLoopback
	}
	if raw&syscall.IFF_POINTOPOINT != 0 {
		f |= net.FlagPointToPoint
	}
	if raw&syscall.IFF_MULTICAST != 0 {
		f |= net.FlagMulticast
	}
	return f
}
```

`m.Data` for an `RTM_NEWLINK` message starts with the `ifinfomsg` header. The `Index` field is at offset 4 and is encoded as `int32` in native byte order. `parseAttrs` takes the slice starting after the header, so it sees only the TLV chain.

### Exercise 3: Address Discovery — addr.go

Create `addr.go`:

```go
//go:build linux

package netlinkmon

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"syscall"
)

// Addr represents an IP address assigned to a network interface.
type Addr struct {
	Prefix  netip.Prefix // e.g. 192.168.1.5/24
	IfIndex int
	Scope   uint8 // RT_SCOPE_*: 0=global, 253=link, 254=host, 255=nowhere
}

// ListAddrs sends RTM_GETADDR and returns assigned IP addresses.
// Pass ifIndex=0 to list addresses for all interfaces.
func (c *Conn) ListAddrs(ifIndex int) ([]Addr, error) {
	if err := c.sendDump(syscall.RTM_GETADDR, syscall.AF_UNSPEC); err != nil {
		return nil, fmt.Errorf("ListAddrs: %w", err)
	}
	msgs, err := c.recvMessages()
	if err != nil {
		return nil, fmt.Errorf("ListAddrs: %w", err)
	}
	var addrs []Addr
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWADDR {
			continue
		}
		a, err := parseAddr(m)
		if err != nil {
			return nil, err
		}
		if ifIndex != 0 && a.IfIndex != ifIndex {
			continue
		}
		addrs = append(addrs, a)
	}
	return addrs, nil
}

func parseAddr(m syscall.NetlinkMessage) (Addr, error) {
	if len(m.Data) < syscall.SizeofIfAddrmsg {
		return Addr{}, fmt.Errorf("parseAddr: short ifaddrmsg (%d bytes)", len(m.Data))
	}
	// ifaddrmsg layout (8 bytes):
	//   [0] Family    uint8
	//   [1] Prefixlen uint8
	//   [2] Flags     uint8  (IFA_F_*)
	//   [3] Scope     uint8
	//   [4:8] Index   uint32
	family := m.Data[0]
	prefixLen := m.Data[1]
	scope := m.Data[3]
	ifIdx := int(binary.NativeEndian.Uint32(m.Data[4:]))

	a := Addr{IfIndex: ifIdx, Scope: scope}
	for _, attr := range parseAttrs(m.Data[syscall.SizeofIfAddrmsg:]) {
		if attr.Attr.Type != syscall.IFA_ADDRESS {
			continue
		}
		ip, ok := bytesToAddr(family, attr.Value)
		if ok {
			a.Prefix = netip.PrefixFrom(ip, int(prefixLen))
		}
		break
	}
	return a, nil
}

// bytesToAddr converts a kernel address byte slice to netip.Addr.
// family selects AF_INET (4 bytes) or AF_INET6 (16 bytes).
func bytesToAddr(family uint8, b []byte) (netip.Addr, bool) {
	switch family {
	case syscall.AF_INET:
		if len(b) == 4 {
			return netip.AddrFrom4([4]byte(b)), true
		}
	case syscall.AF_INET6:
		if len(b) == 16 {
			return netip.AddrFrom16([16]byte(b)), true
		}
	}
	return netip.Addr{}, false
}
```

`IFA_ADDRESS` holds the interface address. For point-to-point links, `IFA_LOCAL` holds the local address and `IFA_ADDRESS` holds the remote peer; for Ethernet both are the same.

### Exercise 4: Route Inspection — route.go

Create `route.go`:

```go
//go:build linux

package netlinkmon

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"syscall"
)

// Route represents one entry in the kernel routing table.
type Route struct {
	Dst      netip.Prefix // destination network, e.g. 10.0.0.0/8
	Gateway  netip.Addr   // next-hop; zero value if directly connected
	IfIndex  int          // output interface index (RTA_OIF)
	Priority int          // route metric
	Table    uint8        // routing table ID (main=254)
}

// IsDefaultRoute reports whether this is a default route (prefix length zero).
func (r Route) IsDefaultRoute() bool {
	return r.Dst.IsValid() && r.Dst.Bits() == 0
}

// ListRoutes sends RTM_GETROUTE and returns the kernel routing table.
func (c *Conn) ListRoutes() ([]Route, error) {
	if err := c.sendDump(syscall.RTM_GETROUTE, syscall.AF_UNSPEC); err != nil {
		return nil, fmt.Errorf("ListRoutes: %w", err)
	}
	msgs, err := c.recvMessages()
	if err != nil {
		return nil, fmt.Errorf("ListRoutes: %w", err)
	}
	var routes []Route
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWROUTE {
			continue
		}
		r, err := parseRoute(m)
		if err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, nil
}

// sizeofRtMsg is the fixed header size for RTNETLINK route messages.
// rtmsg: Family(1) DstLen(1) SrcLen(1) Tos(1) Table(1) Protocol(1)
//
//	Scope(1) Type(1) Flags(4) = 12 bytes.
const sizeofRtMsg = 12

func parseRoute(m syscall.NetlinkMessage) (Route, error) {
	if len(m.Data) < sizeofRtMsg {
		return Route{}, fmt.Errorf("parseRoute: short rtmsg (%d bytes)", len(m.Data))
	}
	family := m.Data[0]
	dstLen := int(m.Data[1])
	table := m.Data[4]

	r := Route{Table: table}
	for _, attr := range parseAttrs(m.Data[sizeofRtMsg:]) {
		switch attr.Attr.Type {
		case syscall.RTA_DST:
			ip, ok := bytesToAddr(family, attr.Value)
			if ok {
				r.Dst = netip.PrefixFrom(ip, dstLen)
			}
		case syscall.RTA_GATEWAY:
			ip, ok := bytesToAddr(family, attr.Value)
			if ok {
				r.Gateway = ip
			}
		case syscall.RTA_OIF:
			if len(attr.Value) == 4 {
				r.IfIndex = int(binary.NativeEndian.Uint32(attr.Value))
			}
		case syscall.RTA_PRIORITY:
			if len(attr.Value) == 4 {
				r.Priority = int(binary.NativeEndian.Uint32(attr.Value))
			}
		}
	}
	// Routes with no RTA_DST attribute represent the default route; synthesise a
	// valid zero prefix so callers can use IsDefaultRoute reliably.
	if !r.Dst.IsValid() {
		switch family {
		case syscall.AF_INET:
			r.Dst = netip.PrefixFrom(netip.IPv4Unspecified(), dstLen)
		case syscall.AF_INET6:
			r.Dst = netip.PrefixFrom(netip.IPv6Unspecified(), dstLen)
		}
	}
	return r, nil
}
```

### Exercise 5: Event Monitor — monitor.go

Create `monitor.go`:

```go
//go:build linux

package netlinkmon

import (
	"context"
	"fmt"
	"sync"
	"syscall"
)

// DefaultGroups is the pre-shifted bitmask for RTNLGRP_LINK (group 1),
// RTNLGRP_IPV4_IFADDR (group 5), and RTNLGRP_IPV4_ROUTE (group 7).
// Pass to OpenMonitor to subscribe to the most common network events.
const DefaultGroups uint32 = (1 << 0) | // RTNLGRP_LINK
	(1 << 4) | // RTNLGRP_IPV4_IFADDR
	(1 << 6) // RTNLGRP_IPV4_ROUTE

// EventKind classifies a Netlink event notification.
type EventKind uint8

const (
	EventLinkNew  EventKind = iota // interface appeared or changed state
	EventLinkDel                   // interface was removed
	EventAddrNew                   // address assigned to an interface
	EventAddrDel                   // address removed from an interface
	EventRouteNew                  // route added to the routing table
	EventRouteDel                  // route removed from the routing table
)

// String returns the human-readable name of the event kind.
func (k EventKind) String() string {
	switch k {
	case EventLinkNew:
		return "LinkNew"
	case EventLinkDel:
		return "LinkDel"
	case EventAddrNew:
		return "AddrNew"
	case EventAddrDel:
		return "AddrDel"
	case EventRouteNew:
		return "RouteNew"
	case EventRouteDel:
		return "RouteDel"
	default:
		return fmt.Sprintf("Unknown(%d)", k)
	}
}

// Event carries a single Netlink notification from the kernel.
type Event struct {
	Kind EventKind
	// Raw holds the unparsed Netlink message for callers that need
	// deeper decoding (e.g. extracting a Link or Addr from the same message).
	Raw syscall.NetlinkMessage
}

// MonitorConn is a Netlink socket bound to one or more multicast groups.
// Use OpenMonitor to create one, then call Monitor to start receiving events.
type MonitorConn struct {
	fd   int
	once sync.Once
}

// OpenMonitor opens a Netlink socket subscribed to the given RTNLGRP_*
// multicast group bitmask. Use DefaultGroups for the most common events.
func OpenMonitor(groups uint32) (*MonitorConn, error) {
	fd, err := syscall.Socket(
		syscall.AF_NETLINK,
		syscall.SOCK_RAW|syscall.SOCK_CLOEXEC,
		syscall.NETLINK_ROUTE,
	)
	if err != nil {
		return nil, fmt.Errorf("netlinkmon: monitor socket: %w", err)
	}
	lsa := &syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Groups: groups,
	}
	if err := syscall.Bind(fd, lsa); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("netlinkmon: monitor bind: %w", err)
	}
	return &MonitorConn{fd: fd}, nil
}

// Close releases the socket. It is safe to call more than once.
// Closing the socket unblocks any goroutine running Monitor.
func (m *MonitorConn) Close() error {
	var err error
	m.once.Do(func() {
		err = syscall.Close(m.fd)
	})
	return err
}

// Monitor starts a receive loop in a background goroutine and returns a channel
// on which decoded Events are sent. The channel is closed when ctx is cancelled
// or an unrecoverable error occurs.
//
// Cancelling ctx calls Close internally; do not call Close concurrently with
// Monitor unless idempotent cleanup is acceptable.
func (m *MonitorConn) Monitor(ctx context.Context) <-chan Event {
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		// When ctx is cancelled, close the fd so the blocked Recvfrom returns.
		go func() {
			<-ctx.Done()
			_ = m.Close()
		}()
		buf := make([]byte, 65536)
		for {
			n, _, err := syscall.Recvfrom(m.fd, buf, 0)
			if err != nil {
				// EBADF when the fd is closed by cancellation; any other error
				// also terminates the loop cleanly.
				return
			}
			msgs, err := syscall.ParseNetlinkMessage(buf[:n])
			if err != nil {
				return
			}
			for _, msg := range msgs {
				ev, ok := classifyEvent(msg)
				if !ok {
					continue
				}
				select {
				case ch <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch
}

func classifyEvent(m syscall.NetlinkMessage) (Event, bool) {
	var kind EventKind
	switch m.Header.Type {
	case syscall.RTM_NEWLINK:
		kind = EventLinkNew
	case syscall.RTM_DELLINK:
		kind = EventLinkDel
	case syscall.RTM_NEWADDR:
		kind = EventAddrNew
	case syscall.RTM_DELADDR:
		kind = EventAddrDel
	case syscall.RTM_NEWROUTE:
		kind = EventRouteNew
	case syscall.RTM_DELROUTE:
		kind = EventRouteDel
	default:
		return Event{}, false
	}
	return Event{Kind: kind, Raw: m}, true
}
```

`sync.Once` ensures the fd is closed exactly once regardless of whether cancellation or an explicit `Close` call triggers first. The inner goroutine that watches `ctx.Done()` and calls `Close` is the canonical pattern for unblocking a syscall with a context.

### Exercise 6: Tests — netlink_test.go

Create `netlink_test.go`. The pure-function tests run on any Linux system without root. The integration tests require a real kernel and are skipped with `go test -short`.

```go
//go:build linux

package netlinkmon

import (
	"encoding/binary"
	"net"
	"net/netip"
	"syscall"
	"testing"
)

func TestNlAlign(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int
		want int
	}{
		{0, 0},
		{1, 4},
		{3, 4},
		{4, 4},
		{5, 8},
		{16, 16},
		{17, 20},
	}
	for _, tc := range cases {
		if got := nlAlign(tc.in); got != tc.want {
			t.Errorf("nlAlign(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestAppendAttr(t *testing.T) {
	t.Parallel()
	value := []byte{0x01, 0x02, 0x03, 0x04}
	got := appendAttr(nil, 99, value)
	// 4-byte hdr + 4-byte value = 8 bytes, already aligned.
	if len(got) != 8 {
		t.Fatalf("appendAttr len = %d, want 8", len(got))
	}
	gotLen := binary.NativeEndian.Uint16(got[0:])
	gotType := binary.NativeEndian.Uint16(got[2:])
	if gotLen != 8 {
		t.Errorf("attr Len = %d, want 8", gotLen)
	}
	if gotType != 99 {
		t.Errorf("attr Type = %d, want 99", gotType)
	}
	if got[4] != 0x01 || got[5] != 0x02 || got[6] != 0x03 || got[7] != 0x04 {
		t.Errorf("value bytes = %v, want [1 2 3 4]", got[4:])
	}
}

func TestAppendAttrPadsToAlignment(t *testing.T) {
	t.Parallel()
	// 3-byte value → 4 hdr + 3 value + 1 pad = 8 bytes.
	got := appendAttr(nil, 1, []byte{0xAA, 0xBB, 0xCC})
	if len(got) != 8 {
		t.Fatalf("appendAttr 3-byte value: len = %d, want 8", len(got))
	}
	// Length field records the unpadded size (hdr + data = 7).
	gotLen := binary.NativeEndian.Uint16(got[0:])
	if gotLen != 7 {
		t.Errorf("attr Len = %d, want 7", gotLen)
	}
}

func TestParseAttrsRoundtrip(t *testing.T) {
	t.Parallel()
	// Build two attributes and parse them back.
	var buf []byte
	val1 := []byte{0x01, 0x02, 0x03, 0x04}
	val2 := []byte{0xAA, 0xBB}
	buf = appendAttr(buf, 10, val1)
	buf = appendAttr(buf, 20, val2)

	attrs := parseAttrs(buf)
	if len(attrs) != 2 {
		t.Fatalf("parseAttrs: got %d attrs, want 2", len(attrs))
	}
	if attrs[0].Attr.Type != 10 {
		t.Errorf("attrs[0].Type = %d, want 10", attrs[0].Attr.Type)
	}
	if string(attrs[0].Value) != string(val1) {
		t.Errorf("attrs[0].Value = %v, want %v", attrs[0].Value, val1)
	}
	if attrs[1].Attr.Type != 20 {
		t.Errorf("attrs[1].Type = %d, want 20", attrs[1].Attr.Type)
	}
	if string(attrs[1].Value) != string(val2) {
		t.Errorf("attrs[1].Value = %v, want %v", attrs[1].Value, val2)
	}
}

func TestKernelFlagsToNet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  uint32
		want net.Flags
	}{
		{syscall.IFF_UP, net.FlagUp},
		{syscall.IFF_LOOPBACK, net.FlagLoopback},
		{syscall.IFF_UP | syscall.IFF_BROADCAST, net.FlagUp | net.FlagBroadcast},
		{syscall.IFF_MULTICAST, net.FlagMulticast},
		{0, 0},
	}
	for _, tc := range cases {
		got := kernelFlagsToNet(tc.raw)
		if got != tc.want {
			t.Errorf("kernelFlagsToNet(%#x) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestBytesToAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		family uint8
		bytes  []byte
		wantOK bool
		want   netip.Addr
	}{
		{
			name:   "IPv4 valid",
			family: syscall.AF_INET,
			bytes:  []byte{192, 168, 1, 1},
			wantOK: true,
			want:   netip.MustParseAddr("192.168.1.1"),
		},
		{
			name:   "IPv4 wrong length",
			family: syscall.AF_INET,
			bytes:  []byte{192, 168, 1},
			wantOK: false,
		},
		{
			name:   "IPv6 loopback",
			family: syscall.AF_INET6,
			bytes:  []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
			wantOK: true,
			want:   netip.MustParseAddr("::1"),
		},
		{
			name:   "unknown family",
			family: syscall.AF_UNSPEC,
			bytes:  []byte{1, 2, 3, 4},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := bytesToAddr(tc.family, tc.bytes)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("addr = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRouteIsDefaultRoute(t *testing.T) {
	t.Parallel()
	def := Route{Dst: netip.PrefixFrom(netip.IPv4Unspecified(), 0)}
	if !def.IsDefaultRoute() {
		t.Error("0.0.0.0/0 should be a default route")
	}
	specific := Route{Dst: netip.MustParsePrefix("10.0.0.0/8")}
	if specific.IsDefaultRoute() {
		t.Error("10.0.0.0/8 should not be a default route")
	}
}

func TestLinkIsUp(t *testing.T) {
	t.Parallel()
	up := Link{Flags: net.FlagUp}
	if !up.IsUp() {
		t.Error("link with FlagUp should report IsUp=true")
	}
	down := Link{}
	if down.IsUp() {
		t.Error("link without FlagUp should report IsUp=false")
	}
}

func TestEventKindString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		k    EventKind
		want string
	}{
		{EventLinkNew, "LinkNew"},
		{EventLinkDel, "LinkDel"},
		{EventAddrNew, "AddrNew"},
		{EventAddrDel, "AddrDel"},
		{EventRouteNew, "RouteNew"},
		{EventRouteDel, "RouteDel"},
		{EventKind(99), "Unknown(99)"},
	}
	for _, tc := range cases {
		if got := tc.k.String(); got != tc.want {
			t.Errorf("EventKind(%d).String() = %q, want %q", tc.k, got, tc.want)
		}
	}
}

// TestListLinksIntegration requires a Linux kernel with at least the loopback
// interface. Run with: go test -run TestListLinksIntegration -v ./...
// (omit -short to allow it).
func TestListLinksIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in short mode; requires Linux")
	}
	c, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	links, err := c.ListLinks()
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	if len(links) == 0 {
		t.Fatal("expected at least the loopback interface")
	}
	var found bool
	for _, l := range links {
		if l.Index <= 0 {
			t.Errorf("link %q: index %d is invalid", l.Name, l.Index)
		}
		if l.Name == "lo" {
			found = true
			if l.Flags&net.FlagLoopback == 0 {
				t.Errorf("lo: FlagLoopback not set, flags = %v", l.Flags)
			}
		}
	}
	if !found {
		t.Error("loopback interface 'lo' not found")
	}
}

// ExampleConn_ListLinks demonstrates opening a connection and iterating links.
// This example requires a Linux kernel and omits // Output: intentionally.
func ExampleConn_ListLinks() {
	c, err := Open()
	if err != nil {
		panic(err)
	}
	defer c.Close()

	links, err := c.ListLinks()
	if err != nil {
		panic(err)
	}
	for _, l := range links {
		_ = l.Name
		_ = l.MTU
	}
}
```

Your turn: add `TestParseAttrsTruncated` that calls `parseAttrs` with a byte slice whose declared `attrLen` exceeds the slice length and asserts that zero attributes are returned (the `break` guard in `parseAttrs`).

### Exercise 7: Demo — cmd/demo/main.go

Create `cmd/demo/main.go`:

```go
//go:build linux

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/netlinkmon"
)

func main() {
	c, err := netlinkmon.Open()
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer c.Close()

	links, err := c.ListLinks()
	if err != nil {
		log.Fatalf("ListLinks: %v", err)
	}
	fmt.Fprintf(os.Stdout, "Interfaces (%d):\n", len(links))
	for _, l := range links {
		fmt.Fprintf(os.Stdout, "  %2d  %-12s  flags=%-20v  mtu=%d\n",
			l.Index, l.Name, l.Flags, l.MTU)
	}

	addrs, err := c.ListAddrs(0)
	if err != nil {
		log.Fatalf("ListAddrs: %v", err)
	}
	fmt.Fprintf(os.Stdout, "\nAddresses (%d):\n", len(addrs))
	for _, a := range addrs {
		fmt.Fprintf(os.Stdout, "  if=%-3d  %s  scope=%d\n", a.IfIndex, a.Prefix, a.Scope)
	}

	routes, err := c.ListRoutes()
	if err != nil {
		log.Fatalf("ListRoutes: %v", err)
	}
	fmt.Fprintf(os.Stdout, "\nRoutes (%d):\n", len(routes))
	for _, r := range routes {
		gw := r.Gateway.String()
		if !r.Gateway.IsValid() {
			gw = "direct"
		}
		fmt.Fprintf(os.Stdout, "  dst=%-20s  gw=%-15s  if=%d  metric=%d\n",
			r.Dst, gw, r.IfIndex, r.Priority)
	}

	// Monitor network events for 5 seconds (or until SIGINT/SIGTERM).
	fmt.Fprintln(os.Stdout, "\nMonitoring events for 5 s (trigger: ip link set lo down && ip link set lo up):")

	mc, err := netlinkmon.OpenMonitor(netlinkmon.DefaultGroups)
	if err != nil {
		log.Fatalf("OpenMonitor: %v", err)
	}
	defer mc.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	for ev := range mc.Monitor(ctx) {
		fmt.Fprintf(os.Stdout, "  event: %s\n", ev.Kind)
	}
}
```

Run on a Linux host (requires read access to the Netlink socket; no root needed for read-only operations):

```bash
go run ./cmd/demo
```

## Common Mistakes

### Terminating Recvfrom Too Early

Wrong: read one `Recvfrom` response and assume it contains all messages for a dump.

What happens: a dump of many interfaces returns multiple datagrams, each with `NLM_F_MULTI` set. Reading only the first datagram returns a partial result.

Fix: loop until a message with `Type == NLMSG_DONE` is received. The `recvMessages` helper does this; calling `syscall.Recvfrom` directly without the loop is the error.

### Using Big-Endian for Netlink Fields

Wrong: `binary.BigEndian.PutUint32(buf[0:], hdr.Len)`.

What happens: the kernel rejects the request with `EINVAL` or returns garbage because Netlink uses host byte order (little-endian on x86/arm64), not network byte order.

Fix: use `binary.NativeEndian` for all Netlink fields. IP addresses inside payloads are in network byte order, but the `nlmsghdr`, `ifinfomsg`, attribute lengths, and integer attribute values are all native-endian.

### Forgetting 4-Byte Alignment

Wrong: placing the next attribute immediately after `hdrLen + len(value)` bytes.

What happens: the kernel silently ignores misaligned attributes, causing attributes to disappear from the parsed result with no error.

Fix: advance by `nlAlign(hdrLen + len(value))` to the next 4-byte boundary. The `appendAttr` function handles this; manually building attribute chains without `nlAlign` is the error.

### Manipulating Routes or Addresses on the Host's Default Namespace

Wrong: calling `AddAddr` or `AddRoute` in production code or tests against the live host network namespace.

What happens: modifications persist, may break connectivity, and require root. Tests that do this are not idempotent.

Fix: test inside a dedicated network namespace (`ip netns add testns`) or use `unshare --net` to isolate the namespace. Dump-only operations (`ListLinks`, `ListAddrs`, `ListRoutes`) require only read access and are safe to run without a separate namespace.

### Not Checking for NLMSG_ERROR Before Parsing Payload

Wrong: interpreting `m.Data` as `ifinfomsg` when `m.Header.Type` might be `NLMSG_ERROR`.

What happens: the first four bytes of the error payload (the errno) get misread as a family byte, a pad, and a link type, producing a nonsensical `Link` with no error returned to the caller.

Fix: always switch on `m.Header.Type` before parsing `m.Data`. The `recvMessages` function returns an error for `NLMSG_ERROR` responses with non-zero errno, so callers of `ListLinks` / `ListAddrs` / `ListRoutes` never see error messages in the returned slice.

## Verification

On a Linux host (or in a Linux container / VM):

```bash
cd ~/go-exercises/netlinkmon

# Format and vet checks — run on macOS with GOOS=linux for cross-check:
GOOS=linux go vet ./...
test -z "$(gofmt -l .)"

# Unit tests (no root required, pure-function tests only):
go test -short -count=1 -race ./...

# Integration tests (requires Linux, no root needed for reads):
go test -count=1 -race -run TestListLinksIntegration ./...

# Run the demo:
go run ./cmd/demo
```

Add one test of your own: `TestParseAttrsTruncated` — call `parseAttrs` with a byte slice where the first two bytes declare `attrLen = 100` but the slice is only 8 bytes long, then assert that `len(attrs) == 0`.

## Summary

- Netlink is the kernel-userspace IPC for network configuration; `AF_NETLINK` + `NETLINK_ROUTE` is the socket pair for links, addresses, and routes.
- Every message begins with a 16-byte `nlmsghdr`; all lengths and attribute offsets are rounded to 4-byte boundaries with `NLMSG_ALIGN`.
- Dump requests return `NLM_F_MULTI`-flagged messages terminated by `NLMSG_DONE`; the receive loop must run until it sees that terminator.
- Error replies have `Type == NLMSG_ERROR` with a signed int32 errno in the first four bytes; zero errno means ACK.
- Netlink integers are in host byte order (use `encoding/binary.NativeEndian`), not network byte order.
- Multicast subscriptions use a bitmask in `SockaddrNetlink.Groups`; a background goroutine reads events and unblocks by closing the fd when the context is cancelled.

## What's Next

Next: [FUSE Filesystem](../04-fuse-filesystem/04-fuse-filesystem.md).

## Resources

- [pkg.go.dev/syscall — Netlink types and ParseNetlinkMessage](https://pkg.go.dev/syscall#ParseNetlinkMessage)
- [Linux man page: netlink(7)](https://man7.org/linux/man-pages/man7/netlink.7.html)
- [Linux man page: rtnetlink(7)](https://man7.org/linux/man-pages/man7/rtnetlink.7.html)
- [RFC 3549 — Linux Netlink as an IP Services Protocol](https://datatracker.ietf.org/doc/html/rfc3549)
- [Linux kernel Netlink UAPI header](https://github.com/torvalds/linux/blob/master/include/uapi/linux/netlink.h)
