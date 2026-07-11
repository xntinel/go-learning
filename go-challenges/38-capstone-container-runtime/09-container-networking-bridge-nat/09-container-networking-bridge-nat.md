# 9. Container Networking: Bridge and NAT

Connecting multiple containers on the same host requires three distinct subsystems to work together: a Linux bridge acting as a virtual switch, iptables rules for NAT and port forwarding, and an IP address manager (IPAM) that allocates unique addresses from a subnet. Each subsystem has its own failure modes and cleanup requirements; getting the interaction between them right is the hard part. This lesson implements the Docker bridge networking model from scratch using `github.com/vishvananda/netlink` for bridge and veth management and `github.com/coreos/go-iptables/iptables` for firewall rule management.

```text
bridge/
  go.mod
  ipam.go            pure Go: IPPool, PortMapping, sentinel errors
  bridge.go          Linux bridge and veth management (requires root, Linux)
  nat.go             iptables NAT and port-forwarding rules (requires root, Linux)
  manager.go         orchestrates bridge + NAT + IPAM for a container lifecycle
  ipam_test.go       table-driven tests for IPPool and PortMapping
  cmd/demo/main.go   runnable demo: allocate IPs, validate port mappings
```

The IPAM and PortMapping code (`ipam.go`, `ipam_test.go`, `cmd/demo`) are pure Go and can be extracted and run anywhere. The bridge and NAT code requires Linux with root access and the two external libraries.

## Concepts

### The Linux Bridge and veth Model

A Linux bridge (`ip link add br0 type bridge`) is a layer-2 virtual switch implemented in the kernel. It maintains a MAC address table and forwards Ethernet frames between its attached ports. A veth pair is a pair of connected virtual Ethernet interfaces: traffic in one end comes out the other. Docker's bridge driver creates one veth pair per container, places one end (`eth0`) inside the container's network namespace, and attaches the other end to the bridge. The bridge then forwards frames between containers and to the host.

The netlink library exposes this as `netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}` for the bridge and `netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "veth0"}, PeerName: "veth1"}` for the pair. `netlink.LinkSetMaster(veth, bridge)` attaches a veth to the bridge. The kernel manages the MAC table automatically once the link is up.

### iptables Tables, Chains, and Rule Ordering

iptables organizes rules in tables (`filter`, `nat`, `mangle`) and chains (`PREROUTING`, `INPUT`, `FORWARD`, `OUTPUT`, `POSTROUTING`). For bridge networking, three rules are essential:

1. **Masquerade (SNAT)**: in `nat POSTROUTING`, match traffic sourced from the bridge subnet going out any interface except the bridge itself, and rewrite the source IP to the host's outbound IP. This gives containers internet access.
   ```
   iptables -t nat -A POSTROUTING -s 172.20.0.0/24 ! -o br0 -j MASQUERADE
   ```

2. **Port forwarding (DNAT)**: in `nat PREROUTING`, match incoming traffic on a host port and rewrite the destination to the container IP and port.
   ```
   iptables -t nat -A PREROUTING -p tcp --dport 8080 -j DNAT --to-destination 172.20.0.2:80
   ```

3. **FORWARD allow**: in `filter FORWARD`, allow forwarded traffic to and from the bridge interface; without this the kernel drops forwarded packets by default when the FORWARD policy is DROP.
   ```
   iptables -A FORWARD -i br0 -j ACCEPT
   iptables -A FORWARD -o br0 -j ACCEPT
   ```

Rule ordering matters: PREROUTING runs before routing decisions, so DNAT rewrites the destination before the kernel decides which interface handles the packet. POSTROUTING runs after routing, so SNAT rewrites the source after the kernel has chosen the outbound interface.

`AppendUnique` (from go-iptables) inserts a rule only if it does not already exist; this makes rule setup idempotent and safe to call on restart. The inverse operation `Delete` removes one rule at a time; on container removal, each rule added at startup must be explicitly deleted.

### IPAM: Sequential Allocation from a Subnet

An IPAM pool maintains a set of allocated addresses within a CIDR block. The subnet's network address and broadcast address are never allocated. The first host address (`.1`) is reserved for the bridge gateway. Allocation starts at the second host address (`.2`) and increments. `encoding/binary.BigEndian.Uint32` converts a 4-byte IP to an integer for arithmetic, then `binary.BigEndian.PutUint32` converts back.

The pool must be safe for concurrent use because multiple goroutines may start containers at the same time. A single `sync.Mutex` protecting the `allocated` map is sufficient.

### IP Forwarding and the Kernel Sysctl

The kernel does not forward packets between interfaces by default. Container networking requires writing `1` to `/proc/sys/net/ipv4/ip_forward`. Without this, packets arriving on `br0` destined for an external address are dropped by the kernel before reaching the iptables FORWARD chain.

```go
err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644)
```

This setting is per-network-namespace. If the container runtime runs in the host namespace (as it does here), enabling it once is sufficient for all containers on the host. A sysctl change survives container restarts but does not persist across reboots; production runtimes write `net.ipv4.ip_forward=1` to `/etc/sysctl.d/` and call `sysctl --system`.

### Port Forwarding: DNAT in PREROUTING

A `--publish hostPort:containerPort` mapping needs two iptables rules. The DNAT rule in `PREROUTING` rewrites the destination for inbound traffic. A FORWARD rule permits that rewritten traffic to reach the container. Both rules must be installed atomically on container start and removed atomically on container stop. The `PortMapping.Validate` method catches malformed mappings before any kernel state is modified.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/bridge/cmd/demo
cd ~/go-exercises/bridge
go mod init example.com/bridge
go get github.com/vishvananda/netlink@v1.3.0
go get github.com/coreos/go-iptables@v0.8.0
```

### Exercise 1: IPAM and PortMapping Validation (pure Go, testable offline)

Create `ipam.go`. This file has no external dependencies and is the foundation that all other components use.

```go
package bridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
)

var (
	// ErrSubnetFull is returned when the pool has no unallocated addresses.
	ErrSubnetFull = errors.New("bridge: subnet exhausted, no free addresses")
	// ErrAddressNotFound is returned when Release is called for an unknown IP.
	ErrAddressNotFound = errors.New("bridge: address not currently allocated")
	// ErrInvalidPort is returned when a port number is outside [1, 65535].
	ErrInvalidPort = errors.New("bridge: port must be between 1 and 65535")
	// ErrInvalidSubnet is returned when the CIDR string cannot be parsed.
	ErrInvalidSubnet = errors.New("bridge: invalid subnet CIDR")
)

// IPPool allocates IPv4 addresses from a subnet.
// The network address and the first host address (.1) are reserved for the
// bridge gateway; the broadcast address is never assigned.
// IPPool is safe for concurrent use.
type IPPool struct {
	mu        sync.Mutex
	network   *net.IPNet
	gateway   net.IP
	allocated map[string]string // ip string -> containerID
}

// NewIPPool parses cidr (e.g. "172.20.0.0/24") and returns an IPPool whose
// gateway is the first host address in the subnet (e.g. 172.20.0.1).
func NewIPPool(cidr string) (*IPPool, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidSubnet, err)
	}
	gw := cloneIP(network.IP.To4())
	gw[3] = 1
	return &IPPool{
		network:   network,
		gateway:   gw,
		allocated: make(map[string]string),
	}, nil
}

// Gateway returns a copy of the bridge gateway IP (e.g. 172.20.0.1).
func (p *IPPool) Gateway() net.IP {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneIP(p.gateway)
}

// Allocate assigns the next free address in the pool to containerID and
// returns the assigned IP. Addresses are assigned in sequential order starting
// from the second host address (e.g. 172.20.0.2 in a /24).
func (p *IPPool) Allocate(containerID string) (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	base := binary.BigEndian.Uint32(p.network.IP.To4())
	ones, bits := p.network.Mask.Size()
	size := uint32(1) << uint(bits-ones)

	for offset := uint32(2); offset < size-1; offset++ {
		b := make(net.IP, 4)
		binary.BigEndian.PutUint32(b, base+offset)
		if !p.network.Contains(b) {
			break
		}
		key := b.String()
		if _, used := p.allocated[key]; !used {
			p.allocated[key] = containerID
			return b, nil
		}
	}
	return nil, ErrSubnetFull
}

// Release removes the allocation for ip. It returns ErrAddressNotFound if the
// address was not allocated.
func (p *IPPool) Release(ip net.IP) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := ip.To4().String()
	if _, ok := p.allocated[key]; !ok {
		return fmt.Errorf("%w: %s", ErrAddressNotFound, key)
	}
	delete(p.allocated, key)
	return nil
}

// AllocatedCount returns the number of currently allocated addresses.
func (p *IPPool) AllocatedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.allocated)
}

func cloneIP(ip net.IP) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	return out
}

// PortMapping describes a host-to-container port forwarding rule.
type PortMapping struct {
	Protocol      string // "tcp" or "udp"
	HostPort      int
	ContainerPort int
	ContainerIP   net.IP
}

// Validate returns a non-nil error if the mapping is malformed.
func (pm PortMapping) Validate() error {
	if pm.Protocol != "tcp" && pm.Protocol != "udp" {
		return fmt.Errorf("bridge: protocol must be tcp or udp, got %q", pm.Protocol)
	}
	if pm.HostPort < 1 || pm.HostPort > 65535 {
		return fmt.Errorf("host %w: got %d", ErrInvalidPort, pm.HostPort)
	}
	if pm.ContainerPort < 1 || pm.ContainerPort > 65535 {
		return fmt.Errorf("container %w: got %d", ErrInvalidPort, pm.ContainerPort)
	}
	if pm.ContainerIP == nil {
		return errors.New("bridge: container IP is required for port mapping")
	}
	return nil
}
```

### Exercise 2: Linux Bridge and veth Management

Create `bridge.go`. This file requires root on Linux and `github.com/vishvananda/netlink`.

```go
package bridge

import (
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"
)

// BridgeManager manages one Linux bridge device and the veth pairs attached
// to it. Each container gets a dedicated veth pair; one end joins the
// container's network namespace, the other attaches to the bridge.
type BridgeManager struct {
	name   string
	subnet string
	pool   *IPPool
}

// NewBridgeManager creates or reattaches to a Linux bridge named bridgeName
// with the given subnet CIDR. The bridge gateway IP is assigned to the bridge
// interface. Requires root.
func NewBridgeManager(bridgeName, subnet string) (*BridgeManager, error) {
	pool, err := NewIPPool(subnet)
	if err != nil {
		return nil, err
	}
	bm := &BridgeManager{name: bridgeName, subnet: subnet, pool: pool}
	if err := bm.ensureBridge(); err != nil {
		return nil, err
	}
	if err := enableIPForwarding(); err != nil {
		return nil, err
	}
	return bm, nil
}

// ensureBridge creates the bridge device if it does not exist, assigns the
// gateway IP from the pool, and brings the device up. Idempotent.
func (bm *BridgeManager) ensureBridge() error {
	existing, err := netlink.LinkByName(bm.name)
	if err == nil {
		if _, ok := existing.(*netlink.Bridge); !ok {
			return fmt.Errorf("bridge: %s exists but is not a bridge device", bm.name)
		}
		return nil // already up
	}

	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{Name: bm.name},
	}
	if err := netlink.LinkAdd(br); err != nil {
		return fmt.Errorf("bridge: LinkAdd %s: %w", bm.name, err)
	}

	link, err := netlink.LinkByName(bm.name)
	if err != nil {
		return fmt.Errorf("bridge: LinkByName after create: %w", err)
	}

	_, ipNet, _ := net.ParseCIDR(bm.subnet)
	ones, _ := ipNet.Mask.Size()
	gwCIDR := fmt.Sprintf("%s/%d", bm.pool.Gateway(), ones)
	addr, err := netlink.ParseAddr(gwCIDR)
	if err != nil {
		return fmt.Errorf("bridge: ParseAddr %s: %w", gwCIDR, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("bridge: AddrAdd %s: %w", gwCIDR, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bridge: LinkSetUp: %w", err)
	}
	return nil
}

// ConnectContainer creates a veth pair, places the container end
// (named "eth0") inside the given network namespace fd, and attaches the host
// end to the bridge. Returns the host-side veth name. IP assignment on eth0
// inside the container namespace is left to the caller (e.g. via a
// namespace-scoped netlink handle).
func (bm *BridgeManager) ConnectContainer(containerID string, nsFd int) (string, error) {
	hostVeth := "veth-" + containerID[:8]
	peerVeth := "eth0"

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostVeth},
		PeerName:  peerVeth,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return "", fmt.Errorf("bridge: LinkAdd veth %s: %w", hostVeth, err)
	}

	// move peer end into the container namespace
	peer, err := netlink.LinkByName(peerVeth)
	if err != nil {
		return "", fmt.Errorf("bridge: LinkByName %s: %w", peerVeth, err)
	}
	if err := netlink.LinkSetNsFd(peer, nsFd); err != nil {
		return "", fmt.Errorf("bridge: LinkSetNsFd: %w", err)
	}

	// attach host end to the bridge
	bridge, err := netlink.LinkByName(bm.name)
	if err != nil {
		return "", fmt.Errorf("bridge: LinkByName %s: %w", bm.name, err)
	}
	host, err := netlink.LinkByName(hostVeth)
	if err != nil {
		return "", fmt.Errorf("bridge: LinkByName %s: %w", hostVeth, err)
	}
	if err := netlink.LinkSetMaster(host, bridge); err != nil {
		return "", fmt.Errorf("bridge: LinkSetMaster: %w", err)
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return "", fmt.Errorf("bridge: LinkSetUp %s: %w", hostVeth, err)
	}
	return hostVeth, nil
}

// DisconnectContainer removes the host-side veth, which destroys the veth
// pair and removes the container from the bridge.
func (bm *BridgeManager) DisconnectContainer(hostVeth string) error {
	link, err := netlink.LinkByName(hostVeth)
	if err != nil {
		return fmt.Errorf("bridge: DisconnectContainer LinkByName %s: %w", hostVeth, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("bridge: LinkDel %s: %w", hostVeth, err)
	}
	return nil
}

// enableIPForwarding writes 1 to /proc/sys/net/ipv4/ip_forward so that the
// kernel routes packets between the bridge and other interfaces.
func enableIPForwarding() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("bridge: enable ip_forward: %w", err)
	}
	return nil
}
```

### Exercise 3: iptables NAT and Port Forwarding

Create `nat.go`. Each public method is idempotent: rules are added with `AppendUnique` and removed with `Delete`.

```go
package bridge

import (
	"fmt"

	"github.com/coreos/go-iptables/iptables"
)

// NATManager installs and removes iptables rules for a bridge subnet.
type NATManager struct {
	ipt        *iptables.IPTables
	bridgeName string
	subnet     string
}

// NewNATManager returns a NATManager for the named bridge and subnet CIDR.
// Requires root and iptables installed on the host.
func NewNATManager(bridgeName, subnet string) (*NATManager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("nat: iptables.New: %w", err)
	}
	return &NATManager{ipt: ipt, bridgeName: bridgeName, subnet: subnet}, nil
}

// EnsureMasquerade installs a POSTROUTING masquerade rule so that containers
// can reach the internet. The rule is added only if it does not already exist.
func (n *NATManager) EnsureMasquerade() error {
	rule := []string{
		"-s", n.subnet,
		"!", "-o", n.bridgeName,
		"-j", "MASQUERADE",
	}
	if err := n.ipt.AppendUnique("nat", "POSTROUTING", rule...); err != nil {
		return fmt.Errorf("nat: AppendUnique MASQUERADE: %w", err)
	}
	return nil
}

// RemoveMasquerade removes the POSTROUTING masquerade rule for this subnet.
func (n *NATManager) RemoveMasquerade() error {
	rule := []string{
		"-s", n.subnet,
		"!", "-o", n.bridgeName,
		"-j", "MASQUERADE",
	}
	if err := n.ipt.Delete("nat", "POSTROUTING", rule...); err != nil {
		return fmt.Errorf("nat: Delete MASQUERADE: %w", err)
	}
	return nil
}

// EnsureForwardRules installs filter FORWARD rules that allow bridged traffic.
func (n *NATManager) EnsureForwardRules() error {
	for _, rule := range forwardRules(n.bridgeName) {
		if err := n.ipt.AppendUnique("filter", "FORWARD", rule...); err != nil {
			return fmt.Errorf("nat: AppendUnique FORWARD: %w", err)
		}
	}
	return nil
}

// RemoveForwardRules removes the FORWARD rules added by EnsureForwardRules.
func (n *NATManager) RemoveForwardRules() error {
	for _, rule := range forwardRules(n.bridgeName) {
		if err := n.ipt.Delete("filter", "FORWARD", rule...); err != nil {
			return fmt.Errorf("nat: Delete FORWARD: %w", err)
		}
	}
	return nil
}

// AddPortMapping installs a DNAT PREROUTING rule and a FORWARD allow rule for
// the given port mapping. pm must pass Validate before calling this method.
func (n *NATManager) AddPortMapping(pm PortMapping) error {
	if err := pm.Validate(); err != nil {
		return err
	}
	dest := fmt.Sprintf("%s:%d", pm.ContainerIP, pm.ContainerPort)
	dnat := []string{
		"-p", pm.Protocol,
		"--dport", fmt.Sprintf("%d", pm.HostPort),
		"-j", "DNAT",
		"--to-destination", dest,
	}
	if err := n.ipt.AppendUnique("nat", "PREROUTING", dnat...); err != nil {
		return fmt.Errorf("nat: AddPortMapping DNAT: %w", err)
	}
	fwd := []string{
		"-d", pm.ContainerIP.String(),
		"-p", pm.Protocol,
		"--dport", fmt.Sprintf("%d", pm.ContainerPort),
		"-j", "ACCEPT",
	}
	if err := n.ipt.AppendUnique("filter", "FORWARD", fwd...); err != nil {
		return fmt.Errorf("nat: AddPortMapping FORWARD: %w", err)
	}
	return nil
}

// RemovePortMapping removes the DNAT and FORWARD rules for pm.
func (n *NATManager) RemovePortMapping(pm PortMapping) error {
	if err := pm.Validate(); err != nil {
		return err
	}
	dest := fmt.Sprintf("%s:%d", pm.ContainerIP, pm.ContainerPort)
	dnat := []string{
		"-p", pm.Protocol,
		"--dport", fmt.Sprintf("%d", pm.HostPort),
		"-j", "DNAT",
		"--to-destination", dest,
	}
	if err := n.ipt.Delete("nat", "PREROUTING", dnat...); err != nil {
		return fmt.Errorf("nat: RemovePortMapping DNAT: %w", err)
	}
	fwd := []string{
		"-d", pm.ContainerIP.String(),
		"-p", pm.Protocol,
		"--dport", fmt.Sprintf("%d", pm.ContainerPort),
		"-j", "ACCEPT",
	}
	if err := n.ipt.Delete("filter", "FORWARD", fwd...); err != nil {
		return fmt.Errorf("nat: RemovePortMapping FORWARD: %w", err)
	}
	return nil
}

func forwardRules(bridgeName string) [][]string {
	return [][]string{
		{"-i", bridgeName, "-j", "ACCEPT"},
		{"-o", bridgeName, "-j", "ACCEPT"},
	}
}
```

### Exercise 4: Manager Lifecycle

Create `manager.go` to tie together the three subsystems. Each container goes through `Attach` on start and `Detach` on stop.

```go
package bridge

import (
	"fmt"
	"net"
)

// ContainerNet records the network state assigned to a running container.
type ContainerNet struct {
	ContainerID string
	IP          net.IP
	Gateway     net.IP
	HostVeth    string
	Mappings    []PortMapping
}

// Manager orchestrates IPAM, bridge management, and NAT for a set of
// containers sharing one bridge network.
type Manager struct {
	bridge *BridgeManager
	nat    *NATManager
	pool   *IPPool
}

// NewManager creates a Manager for the named bridge and subnet.
// bridgeName is the Linux bridge interface name (e.g. "ctr0").
// subnet is the CIDR block for containers (e.g. "172.20.0.0/24").
func NewManager(bridgeName, subnet string) (*Manager, error) {
	pool, err := NewIPPool(subnet)
	if err != nil {
		return nil, err
	}
	bm, err := NewBridgeManager(bridgeName, subnet)
	if err != nil {
		return nil, err
	}
	nat, err := NewNATManager(bridgeName, subnet)
	if err != nil {
		return nil, err
	}
	if err := nat.EnsureMasquerade(); err != nil {
		return nil, err
	}
	if err := nat.EnsureForwardRules(); err != nil {
		return nil, err
	}
	return &Manager{bridge: bm, nat: nat, pool: pool}, nil
}

// Attach allocates an IP for containerID, creates and connects the veth pair,
// and installs iptables rules for any port mappings. nsFd is the file
// descriptor of the container's network namespace (from /proc/self/fd/...).
func (m *Manager) Attach(containerID string, nsFd int, mappings []PortMapping) (*ContainerNet, error) {
	ip, err := m.pool.Allocate(containerID)
	if err != nil {
		return nil, fmt.Errorf("manager: Attach %s: %w", containerID, err)
	}

	hostVeth, err := m.bridge.ConnectContainer(containerID, nsFd)
	if err != nil {
		_ = m.pool.Release(ip)
		return nil, fmt.Errorf("manager: ConnectContainer: %w", err)
	}

	for i, pm := range mappings {
		pm.ContainerIP = ip
		mappings[i] = pm
		if err := m.nat.AddPortMapping(pm); err != nil {
			_ = m.bridge.DisconnectContainer(hostVeth)
			_ = m.pool.Release(ip)
			return nil, fmt.Errorf("manager: AddPortMapping: %w", err)
		}
	}

	return &ContainerNet{
		ContainerID: containerID,
		IP:          ip,
		Gateway:     m.pool.Gateway(),
		HostVeth:    hostVeth,
		Mappings:    mappings,
	}, nil
}

// Detach removes all iptables rules for the container, disconnects the veth
// pair, and returns the IP to the pool. It is safe to call even if some steps
// already failed.
func (m *Manager) Detach(cn *ContainerNet) error {
	var errs []error
	for _, pm := range cn.Mappings {
		if err := m.nat.RemovePortMapping(pm); err != nil {
			errs = append(errs, err)
		}
	}
	if err := m.bridge.DisconnectContainer(cn.HostVeth); err != nil {
		errs = append(errs, err)
	}
	if err := m.pool.Release(cn.IP); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("manager: Detach %s: %v", cn.ContainerID, errs)
	}
	return nil
}
```

### Exercise 5: Test the Contract (extractable, runs offline)

Create `ipam_test.go`. These tests cover the IPAM and PortMapping code from `ipam.go` and do not require Linux or root.

```go
package bridge

import (
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestNewIPPoolGateway(t *testing.T) {
	t.Parallel()

	pool, err := NewIPPool("172.20.0.0/24")
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	if got := pool.Gateway().String(); got != "172.20.0.1" {
		t.Fatalf("Gateway = %s, want 172.20.0.1", got)
	}
}

func TestNewIPPoolRejectsInvalidCIDR(t *testing.T) {
	t.Parallel()

	_, err := NewIPPool("not-a-cidr")
	if !errors.Is(err, ErrInvalidSubnet) {
		t.Fatalf("err = %v, want ErrInvalidSubnet", err)
	}
}

func TestAllocateSequential(t *testing.T) {
	t.Parallel()

	pool, _ := NewIPPool("172.20.0.0/24")

	ip1, err := pool.Allocate("c1")
	if err != nil {
		t.Fatalf("first Allocate: %v", err)
	}
	if ip1.String() != "172.20.0.2" {
		t.Fatalf("first allocation = %s, want 172.20.0.2", ip1)
	}

	ip2, err := pool.Allocate("c2")
	if err != nil {
		t.Fatalf("second Allocate: %v", err)
	}
	if ip2.String() != "172.20.0.3" {
		t.Fatalf("second allocation = %s, want 172.20.0.3", ip2)
	}
}

func TestAllocateAfterRelease(t *testing.T) {
	t.Parallel()

	pool, _ := NewIPPool("172.20.0.0/24")
	ip, _ := pool.Allocate("c1")
	if err := pool.Release(ip); err != nil {
		t.Fatalf("Release: %v", err)
	}
	ip2, err := pool.Allocate("c2")
	if err != nil {
		t.Fatalf("Allocate after Release: %v", err)
	}
	if ip2.String() != "172.20.0.2" {
		t.Fatalf("ip2 = %s, want 172.20.0.2 (reused)", ip2)
	}
}

func TestReleaseUnknownAddress(t *testing.T) {
	t.Parallel()

	pool, _ := NewIPPool("172.20.0.0/24")
	err := pool.Release(net.ParseIP("172.20.0.50"))
	if !errors.Is(err, ErrAddressNotFound) {
		t.Fatalf("err = %v, want ErrAddressNotFound", err)
	}
}

func TestSubnetExhaustion(t *testing.T) {
	t.Parallel()

	// A /30 has 4 addresses: .0 network, .1 gateway, .2 first host, .3 broadcast.
	// Only .2 is allocatable; the broadcast (.3) is excluded.
	pool, _ := NewIPPool("10.0.0.0/30")
	_, err := pool.Allocate("c1")
	if err != nil {
		t.Fatalf("first allocation in /30: %v", err)
	}
	_, err = pool.Allocate("c2")
	if !errors.Is(err, ErrSubnetFull) {
		t.Fatalf("err = %v, want ErrSubnetFull", err)
	}
}

func TestAllocatedCount(t *testing.T) {
	t.Parallel()

	pool, _ := NewIPPool("172.20.0.0/24")
	if pool.AllocatedCount() != 0 {
		t.Fatal("AllocatedCount should be 0 initially")
	}
	ip, _ := pool.Allocate("c1")
	if pool.AllocatedCount() != 1 {
		t.Fatal("AllocatedCount should be 1 after one allocation")
	}
	_ = pool.Release(ip)
	if pool.AllocatedCount() != 0 {
		t.Fatal("AllocatedCount should be 0 after release")
	}
}

func TestPortMappingValidatePortBounds(t *testing.T) {
	t.Parallel()

	cip := net.ParseIP("172.20.0.2")
	cases := []struct {
		name string
		pm   PortMapping
	}{
		{"host port zero", PortMapping{Protocol: "tcp", HostPort: 0, ContainerPort: 80, ContainerIP: cip}},
		{"host port too large", PortMapping{Protocol: "tcp", HostPort: 65536, ContainerPort: 80, ContainerIP: cip}},
		{"host port negative", PortMapping{Protocol: "tcp", HostPort: -1, ContainerPort: 80, ContainerIP: cip}},
		{"container port zero", PortMapping{Protocol: "tcp", HostPort: 8080, ContainerPort: 0, ContainerIP: cip}},
		{"container port too large", PortMapping{Protocol: "tcp", HostPort: 8080, ContainerPort: 65536, ContainerIP: cip}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.pm.Validate()
			if !errors.Is(err, ErrInvalidPort) {
				t.Fatalf("Validate() err = %v, want ErrInvalidPort", err)
			}
		})
	}
}

func TestPortMappingValidateAcceptsValid(t *testing.T) {
	t.Parallel()

	pm := PortMapping{
		Protocol:      "tcp",
		HostPort:      8080,
		ContainerPort: 80,
		ContainerIP:   net.ParseIP("172.20.0.2"),
	}
	if err := pm.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error: %v", err)
	}
}

func TestPortMappingValidateRejectsUnknownProtocol(t *testing.T) {
	t.Parallel()

	pm := PortMapping{Protocol: "sctp", HostPort: 8080, ContainerPort: 80, ContainerIP: net.ParseIP("172.20.0.2")}
	if err := pm.Validate(); err == nil {
		t.Fatal("Validate() should reject unknown protocol sctp")
	}
}

func TestPortMappingValidateRequiresContainerIP(t *testing.T) {
	t.Parallel()

	pm := PortMapping{Protocol: "tcp", HostPort: 8080, ContainerPort: 80, ContainerIP: nil}
	if err := pm.Validate(); err == nil {
		t.Fatal("Validate() should reject nil ContainerIP")
	}
}

// Your turn: add TestAllocateConcurrent that launches 10 goroutines each
// calling pool.Allocate, then verifies no two goroutines received the same IP.

func ExampleIPPool_Allocate() {
	pool, _ := NewIPPool("172.20.0.0/24")
	ip, _ := pool.Allocate("container-1")
	fmt.Println(ip)
	// Output: 172.20.0.2
}

func ExampleIPPool_Gateway() {
	pool, _ := NewIPPool("10.100.0.0/16")
	fmt.Println(pool.Gateway())
	// Output: 10.100.0.1
}
```

### Exercise 6: Demo

Create `cmd/demo/main.go`. This program exercises the exported API of `ipam.go` and requires no Linux privileges.

```go
package main

import (
	"fmt"
	"log"
	"net"
	"os"

	"example.com/bridge"
)

func main() {
	pool, err := bridge.NewIPPool("172.20.0.0/24")
	if err != nil {
		log.Fatalf("NewIPPool: %v", err)
	}
	fmt.Fprintf(os.Stdout, "bridge gateway: %s\n", pool.Gateway())

	containers := []string{"nginx-1", "redis-2", "api-3"}
	ips := make([]net.IP, 0, len(containers))

	for _, id := range containers {
		ip, err := pool.Allocate(id)
		if err != nil {
			log.Fatalf("Allocate %s: %v", id, err)
		}
		fmt.Fprintf(os.Stdout, "  allocated %s -> %s\n", id, ip)
		ips = append(ips, ip)
	}
	fmt.Fprintf(os.Stdout, "allocated count: %d\n", pool.AllocatedCount())

	// validate a port mapping
	pm := bridge.PortMapping{
		Protocol:      "tcp",
		HostPort:      8080,
		ContainerPort: 80,
		ContainerIP:   ips[0],
	}
	if err := pm.Validate(); err != nil {
		log.Fatalf("PortMapping.Validate: %v", err)
	}
	fmt.Fprintf(os.Stdout, "port mapping 0.0.0.0:%d -> %s:%d OK\n",
		pm.HostPort, pm.ContainerIP, pm.ContainerPort)

	// release one container and reallocate
	_ = pool.Release(ips[0])
	fmt.Fprintf(os.Stdout, "released %s, allocated count: %d\n", ips[0], pool.AllocatedCount())
	reused, _ := pool.Allocate("proxy-4")
	fmt.Fprintf(os.Stdout, "  reallocated -> %s (reused)\n", reused)
}
```

Run it with:

```bash
go run ./cmd/demo
```

Expected output (addresses are deterministic):

```text
bridge gateway: 172.20.0.1
  allocated nginx-1 -> 172.20.0.2
  allocated redis-2 -> 172.20.0.3
  allocated api-3 -> 172.20.0.4
allocated count: 3
port mapping 0.0.0.0:8080 -> 172.20.0.2:80 OK
released 172.20.0.2, allocated count: 2
  reallocated -> 172.20.0.2 (reused)
```

## Common Mistakes

### Not enabling IP forwarding

Wrong: configure the bridge and iptables rules but skip the `/proc/sys/net/ipv4/ip_forward` write. Containers can reach the bridge gateway but not the internet; packets arriving on `br0` destined for an external address are silently dropped by the kernel before the iptables POSTROUTING chain runs.

Fix: call `enableIPForwarding()` in `NewBridgeManager` before installing any iptables rules. Verify with `cat /proc/sys/net/ipv4/ip_forward` (must print `1`).

### Using Append instead of AppendUnique for iptables rules

Wrong: `ipt.Append("nat", "POSTROUTING", rule...)` on every runtime startup adds a duplicate masquerade rule. Each duplicate independently matches traffic, causing double NAT and connection issues at scale.

Fix: `ipt.AppendUnique(...)` checks whether the rule already exists in the chain before inserting it. The check is atomic within the iptables library; duplicate rules are never installed.

### Installing DNAT in OUTPUT instead of PREROUTING

Wrong: adding the DNAT rule to the `nat OUTPUT` chain. OUTPUT runs for packets originating on the host; external traffic arriving on the network interface never traverses OUTPUT.

Fix: DNAT for inbound port forwarding must go in `nat PREROUTING`. PREROUTING runs before routing decisions; the kernel sees the rewritten destination and routes the packet to the container.

### Forgetting the FORWARD allow rule after adding DNAT

Wrong: installing the DNAT rule in PREROUTING without a matching ACCEPT in the FORWARD chain. With a default FORWARD policy of DROP, the DNAT rewrite succeeds but the packet is then dropped because no FORWARD rule permits it.

Fix: after each `AddPortMapping` DNAT rule, also install a FORWARD ACCEPT rule that matches the container IP and port. Both rules must be removed together in `RemovePortMapping`.

### Treating the /30 broadcast as allocatable

Wrong: a /30 has 4 addresses and `size-1 = 3`, but `offset < size-1` means the loop exits before reaching offset 3. Trying to subtract 1 from the size and use that as the upper bound for the container space causes an off-by-one that allocates the broadcast address (`.3` in a /30), which the kernel ignores for unicast traffic.

Fix: the loop condition `offset < size-1` is correct: it excludes both the network address (reserved by starting at offset 2) and the broadcast (excluded by the `< size-1` upper bound). A /30 therefore has exactly one allocatable address.

### Skipping cleanup on container removal

Wrong: calling `Detach` only on clean shutdown, not on error paths. Leaked veth pairs are not cleaned up when the bridge is deleted and accumulate as kernel state across reboots.

Fix: `Detach` collects all cleanup errors into a slice and returns them all rather than stopping at the first. Call it in a `defer` or from a signal handler in the container runtime to ensure it runs even when `Attach` fails partway through.

## Verification

The IPAM and PortMapping tests run offline on any platform:

```bash
cd ~/go-exercises/bridge
test -z "$(gofmt -l .)"
go vet ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

The bridge and NAT code requires a Linux host with root access, the two external packages (`go get`), and the `iptables` binary:

```bash
sudo go test -count=1 -run TestBridge ./...   # requires Linux + root
sudo go test -count=1 -run TestNAT ./...      # requires Linux + root + iptables
```

After running the full manager on a Linux host, verify the bridge driver end-to-end:

```bash
# confirm bridge exists and has correct gateway IP
ip addr show ctr0

# confirm masquerade rule is present
sudo iptables -t nat -L POSTROUTING -n -v | grep MASQUERADE

# confirm FORWARD rules allow bridge traffic
sudo iptables -L FORWARD -n -v | grep ctr0

# from inside a container namespace, confirm internet reachability
sudo nsenter --net=/proc/<pid>/ns/net -- ping -c1 8.8.8.8

# confirm port forwarding: curl from the host to a container service
curl -sf http://localhost:8080/
```

Add `TestAllocateConcurrent` (the "your turn" exercise): launch 10 goroutines each calling `pool.Allocate`, collect results into a slice protected by a mutex, and assert that no two goroutines received the same IP string.

## Summary

- A Linux bridge is a layer-2 virtual switch; veth pairs connect container network namespaces to the bridge.
- `netlink.LinkSetMaster(veth, bridge)` attaches a veth to the bridge; `netlink.LinkDel` removes it and destroys the pair.
- iptables POSTROUTING masquerade (SNAT) gives containers outbound internet access; PREROUTING DNAT implements port forwarding for inbound traffic.
- `AppendUnique` makes rule installation idempotent; every `AddPortMapping` call has a symmetric `RemovePortMapping` for cleanup.
- IP forwarding must be enabled via `/proc/sys/net/ipv4/ip_forward`; without it, the kernel drops forwarded packets before iptables sees them.
- IPAM uses `encoding/binary` integer arithmetic over the subnet base address to allocate sequentially while skipping the gateway and broadcast.
- A `sync.Mutex` over the `allocated` map is sufficient for concurrent container starts.

## What's Next

Next: [Full OCI Container Runtime](../10-full-oci-container-runtime/10-full-oci-container-runtime.md).

## Resources

- [Linux bridge and veth: kernel networking docs](https://www.kernel.org/doc/html/latest/networking/bridge.html) -- authoritative kernel bridge documentation
- [vishvananda/netlink Go library](https://pkg.go.dev/github.com/vishvananda/netlink) -- Go bindings for rtnetlink; the source of all LinkAdd/LinkSetMaster/AddrAdd signatures used here
- [coreos/go-iptables](https://pkg.go.dev/github.com/coreos/go-iptables/iptables) -- Go bindings for iptables; AppendUnique, Delete, and Exists signatures
- [CNI bridge plugin source](https://github.com/containernetworking/plugins/tree/main/plugins/main/bridge) -- production reference implementation of the same bridge driver pattern
- [net package: ParseCIDR, IPNet](https://pkg.go.dev/net#ParseCIDR) -- stdlib IP arithmetic used in IPPool.Allocate
