# 3. Network Namespace and Veth Pairs

A container with its own process tree and filesystem still shares the host network stack by default. `CLONE_NEWNET` gives each container its own stack — its own interfaces, routing table, netfilter rules, and socket namespace — but the new stack starts completely empty: not even loopback exists. Connecting that empty namespace to the outside world requires a **veth pair**, a virtual Ethernet cable whose two ends live in different namespaces. This lesson builds a Go package that creates, moves, configures, and cleans up that pair without shelling out to `ip`.

The hard part is the race between parent and child: the child's `eth0` does not exist until the parent creates the veth pair and moves the peer end into the child namespace. Without explicit synchronization the child tries to configure a non-existent interface and fails. You will build the synchronization pipe, the host-side setup function, and the container-side configuration as a testable Go package.

```text
vethns/
  go.mod
  vethns.go
  setup.go
  container.go
  vethns_test.go
  cmd/
    demo/
      main.go
```

## Concepts

### Network Namespace Isolation

`clone(2)` with `CLONE_NEWNET` gives the new process its own copy of the kernel's network stack. Each namespace holds:

- A set of network interfaces (none by default; even `lo` starts DOWN)
- An independent IPv4/IPv6 routing table
- Its own netfilter/iptables chains
- Its own port number space (two processes in different namespaces can both bind `:8080`)

The namespace persists as long as at least one process lives in it or an open file descriptor references it. Opening `/proc/<pid>/ns/net` keeps the namespace alive and provides the fd needed to move interfaces into it.

### Veth Pairs: Virtual Ethernet Cables

`ip link add veth0 type veth peer name eth0` creates two network interfaces atomically joined in a point-to-point Ethernet link. Packets sent out of one end arrive at the other exactly as they would on a physical cable. Moving one end into a container namespace builds the tunnel:

```
host namespace              container namespace
  veth-demo0 <-----------> eth0
  10.10.10.1/24             10.10.10.2/24
```

The kernel enforces one invariant: deleting either end of the pair removes the other. Cleanup therefore requires only one `LinkDel` call on the host side.

### Netlink: The Kernel's Configuration Interface

All interface and route configuration happens through netlink sockets (`AF_NETLINK`, `NETLINK_ROUTE`). Tools like `ip` and `brctl` are thin wrappers around netlink messages. The `github.com/vishvananda/netlink` library serializes those messages as idiomatic Go function calls.

Core operations used in this lesson:

| Function | Effect |
|---|---|
| `netlink.LinkAdd(&netlink.Veth{...})` | Creates veth pair atomically in the current namespace |
| `netlink.LinkByName(name)` | Looks up a link by interface name |
| `netlink.LinkSetNsFd(link, fd)` | Moves a link into the namespace identified by fd |
| `netlink.AddrAdd(link, addr)` | Assigns an IP/prefix to a link |
| `netlink.LinkSetUp(link)` | Transitions an interface from DOWN to UP |
| `netlink.RouteAdd(route)` | Installs a kernel routing entry |
| `netlink.LinkDel(link)` | Deletes a link (and its veth peer) |

### The Pipe Synchronization Protocol

The container process starts before the parent has created the veth pair. If it calls `ContainerSetup` immediately, `eth0` does not exist and every netlink call returns ENODEV. The fix is a synchronization pipe:

1. Parent creates `(r, w)` before forking.
2. Parent passes `r` to the child as an extra file descriptor (fd 3).
3. Child blocks on `r.Read(buf)`.
4. Parent creates the veth pair, moves the peer into the child namespace, configures the host side.
5. Parent writes one byte to `w`, closing it.
6. Child's read unblocks; child configures its own interfaces and runs its workload.
7. Parent calls `child.Wait()`, then removes the host-side veth.

The read-before-proceed ordering guarantees that `eth0` exists in the child namespace before `ContainerSetup` is called.

### Goroutine Safety When Entering a Namespace

Linux namespaces are per-OS-thread. Go goroutines are multiplexed across threads by the runtime. If a goroutine calls `unix.Setns(fd, unix.CLONE_NEWNET)` to enter a foreign namespace, the runtime may schedule subsequent goroutine code on a different thread that is still in the original namespace, silently sending netlink messages to the wrong namespace.

The safe pattern when a goroutine must operate in a foreign namespace:

```go
runtime.LockOSThread()
defer runtime.UnlockOSThread()
unix.Setns(int(foreignFd), unix.CLONE_NEWNET)
// netlink operations here run on the locked thread
unix.Setns(int(origFd), unix.CLONE_NEWNET) // restore before unlock
```

This lesson avoids the problem entirely: `ContainerSetup` runs inside the child process, which is already in the correct namespace after `Start()` with `CLONE_NEWNET`. No explicit `Setns` call is needed.

### IFNAMSIZ and Interface Naming

Linux enforces a 15-character limit on interface names (IFNAMSIZ = 16 including the null terminator). A name like `veth-containerid-uuid` silently truncates or is rejected. A safe naming scheme: host veth = `"veth-"` + the first 10 characters of the container ID; container veth = `"eth0"`. Any scheme must be enforced in code and pinned by a test.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/38-capstone-container-runtime/03-network-namespace-veth/03-network-namespace-veth/cmd/demo
cd go-solutions/38-capstone-container-runtime/03-network-namespace-veth/03-network-namespace-veth
go get github.com/vishvananda/netlink@latest
```

This is a library with a separate demo. The library is verified with `go test`; the demo requires root on Linux.

### Exercise 1: Config, Validation, and the parseCIDR Helper

Create `vethns.go`:

```go
//go:build linux

package vethns

import (
	"errors"
	"fmt"
	"net"
)

// Sentinel errors returned by Config.Validate.
var (
	ErrEmptyID     = errors.New("vethns: id is required")
	ErrEmptyVeth   = errors.New("vethns: veth name is required")
	ErrNameTooLong = errors.New("vethns: interface name exceeds IFNAMSIZ (15 chars)")
	ErrInvalidCIDR = errors.New("vethns: invalid CIDR")
	ErrInvalidGW   = errors.New("vethns: invalid gateway IP")
)

// Config describes the veth pair and IP addressing for one container network namespace.
type Config struct {
	// ID uniquely identifies the container; used for naming and logging.
	ID string
	// HostVeth is the name of the veth end that remains in the host namespace.
	// Must be 15 characters or fewer (IFNAMSIZ).
	HostVeth string
	// ContVeth is the name of the veth end moved into the container namespace.
	// Must be 15 characters or fewer (IFNAMSIZ).
	ContVeth string
	// HostCIDR is the host-end IP address with prefix length (e.g. "10.10.10.1/24").
	HostCIDR string
	// ContCIDR is the container-end IP address with prefix length (e.g. "10.10.10.2/24").
	ContCIDR string
	// Gateway is the IPv4 address the container uses as its default gateway.
	// Normally the host-end IP without prefix (e.g. "10.10.10.1").
	Gateway string
}

// DefaultConfig returns a Config with sensible defaults for the given id.
// The host veth name is "veth-" + id truncated to 10 chars (keeping total <= 15).
// The container veth is always "eth0".
//
// DefaultConfig does not allocate unique subnets. Production code must assign
// non-overlapping CIDR ranges per container.
func DefaultConfig(id string) Config {
	const maxSuffix = 10 // "veth-" = 5 chars; IFNAMSIZ - 5 = 10
	suffix := id
	if len(suffix) > maxSuffix {
		suffix = suffix[:maxSuffix]
	}
	return Config{
		ID:       id,
		HostVeth: "veth-" + suffix,
		ContVeth: "eth0",
		HostCIDR: "10.10.10.1/24",
		ContCIDR: "10.10.10.2/24",
		Gateway:  "10.10.10.1",
	}
}

// Validate reports the first validation error found in c, or nil if c is valid.
func (c Config) Validate() error {
	if c.ID == "" {
		return ErrEmptyID
	}
	if c.HostVeth == "" || c.ContVeth == "" {
		return ErrEmptyVeth
	}
	if len(c.HostVeth) > 15 {
		return fmt.Errorf("%w: %q (%d chars)", ErrNameTooLong, c.HostVeth, len(c.HostVeth))
	}
	if len(c.ContVeth) > 15 {
		return fmt.Errorf("%w: %q (%d chars)", ErrNameTooLong, c.ContVeth, len(c.ContVeth))
	}
	if _, err := parseCIDR(c.HostCIDR); err != nil {
		return fmt.Errorf("%w (host): %v", ErrInvalidCIDR, err)
	}
	if _, err := parseCIDR(c.ContCIDR); err != nil {
		return fmt.Errorf("%w (cont): %v", ErrInvalidCIDR, err)
	}
	if net.ParseIP(c.Gateway) == nil {
		return fmt.Errorf("%w: %q", ErrInvalidGW, c.Gateway)
	}
	return nil
}

// parseCIDR parses a CIDR string and returns a *net.IPNet with the host address
// preserved. net.ParseCIDR("10.10.10.1/24") returns IPNet.IP = 10.10.10.0 (the
// network address); this helper restores the host address so AddrAdd assigns
// the correct IP to the interface.
func parseCIDR(cidr string) (*net.IPNet, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	ipnet.IP = ip.To4()
	return ipnet, nil
}
```

`parseCIDR` is the only non-obvious utility. The stdlib's `net.ParseCIDR` intentionally zeros host bits in `IPNet.IP` to return the network address; assigning that to an interface would give the wrong IP. The fix copies the original `ip` back before returning.

### Exercise 2: Host-Side Setup, Container-Side Setup, and Teardown

Create `setup.go`:

```go
//go:build linux

package vethns

import (
	"fmt"
	"os"

	"github.com/vishvananda/netlink"
)

// HostSetup creates the veth pair, moves the container end into the network
// namespace of the process with the given pid, then configures and brings up
// the host end. It must be called from the host network namespace.
//
// pid must be the PID of a process already running inside CLONE_NEWNET.
// The child must block on the synchronization pipe until HostSetup returns.
func HostSetup(cfg Config, pid int) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	// Create both ends of the veth pair in the current (host) namespace.
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: cfg.HostVeth},
		PeerName:  cfg.ContVeth,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("vethns: LinkAdd(%s<->%s): %w", cfg.HostVeth, cfg.ContVeth, err)
	}

	// Open the container's network namespace fd via /proc.
	nsPath := fmt.Sprintf("/proc/%d/ns/net", pid)
	nsFile, err := os.Open(nsPath)
	if err != nil {
		_ = netlink.LinkDel(veth) // best-effort: deletes both ends
		return fmt.Errorf("vethns: open ns %s: %w", nsPath, err)
	}
	defer nsFile.Close()

	// Fetch the peer link by name before moving it. After LinkSetNsFd the peer
	// disappears from the current namespace and cannot be looked up here.
	peer, err := netlink.LinkByName(cfg.ContVeth)
	if err != nil {
		_ = netlink.LinkDel(veth)
		return fmt.Errorf("vethns: LinkByName(%s): %w", cfg.ContVeth, err)
	}

	// Move the container end into the child's network namespace.
	if err := netlink.LinkSetNsFd(peer, int(nsFile.Fd())); err != nil {
		_ = netlink.LinkDel(veth)
		return fmt.Errorf("vethns: LinkSetNsFd(%s): %w", cfg.ContVeth, err)
	}

	// After LinkSetNsFd the peer link object is stale. Re-fetch the host end.
	host, err := netlink.LinkByName(cfg.HostVeth)
	if err != nil {
		return fmt.Errorf("vethns: LinkByName(%s) after move: %w", cfg.HostVeth, err)
	}

	// Parse and assign the host IP address, preserving the host bits.
	hostAddr, err := parseCIDR(cfg.HostCIDR)
	if err != nil {
		return fmt.Errorf("vethns: parse host CIDR: %w", err)
	}
	if err := netlink.AddrAdd(host, &netlink.Addr{IPNet: hostAddr}); err != nil {
		return fmt.Errorf("vethns: AddrAdd(%s, %s): %w", cfg.HostVeth, cfg.HostCIDR, err)
	}

	// Bring the host end up.
	if err := netlink.LinkSetUp(host); err != nil {
		return fmt.Errorf("vethns: LinkSetUp(%s): %w", cfg.HostVeth, err)
	}

	return nil
}

// Teardown removes the host-side veth interface. Because veth peers are
// symmetric, removing one end automatically removes the other.
// Teardown is idempotent: if the interface is already gone it returns nil.
func Teardown(cfg Config) error {
	link, err := netlink.LinkByName(cfg.HostVeth)
	if err != nil {
		// Link not found means it was already removed. Treat as success.
		return nil
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("vethns: Teardown LinkDel(%s): %w", cfg.HostVeth, err)
	}
	return nil
}
```

Create `container.go`:

```go
//go:build linux

package vethns

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// ContainerSetup configures networking from inside a container's network
// namespace. It must be called by the container process after unblocking
// from the synchronization pipe — the interface does not exist before that.
//
// ContainerSetup brings up loopback, assigns ContCIDR to ContVeth,
// and installs a default route via Gateway.
func ContainerSetup(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	// Bring up loopback. Without this, any bind or connect to 127.0.0.1 fails.
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("vethns: get lo: %w", err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return fmt.Errorf("vethns: lo up: %w", err)
	}

	// Fetch the container veth (moved here by the parent in HostSetup).
	eth, err := netlink.LinkByName(cfg.ContVeth)
	if err != nil {
		return fmt.Errorf("vethns: get %s: %w", cfg.ContVeth, err)
	}

	// Assign the container IP address, preserving the host bits.
	contAddr, err := parseCIDR(cfg.ContCIDR)
	if err != nil {
		return fmt.Errorf("vethns: parse cont CIDR: %w", err)
	}
	if err := netlink.AddrAdd(eth, &netlink.Addr{IPNet: contAddr}); err != nil {
		return fmt.Errorf("vethns: AddrAdd(%s, %s): %w", cfg.ContVeth, cfg.ContCIDR, err)
	}
	if err := netlink.LinkSetUp(eth); err != nil {
		return fmt.Errorf("vethns: %s up: %w", cfg.ContVeth, err)
	}

	// Install a default route via the host-end IP (the gateway).
	gw := net.ParseIP(cfg.Gateway)
	if gw == nil {
		return fmt.Errorf("%w: %q", ErrInvalidGW, cfg.Gateway)
	}
	_, defaultDst, _ := net.ParseCIDR("0.0.0.0/0")
	route := &netlink.Route{
		LinkIndex: eth.Attrs().Index,
		Dst:       defaultDst,
		Gw:        gw,
	}
	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("vethns: RouteAdd default via %s: %w", cfg.Gateway, err)
	}

	return nil
}
```

### Exercise 3: Tests and Demo

The validation tests (Exercises 1-2 above) run without root. The integration tests require root and a Linux kernel that allows network namespace operations.

Create `vethns_test.go`:

```go
//go:build linux

package vethns

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

// requireRoot skips the test if the process lacks CAP_NET_ADMIN.
// Veth and namespace operations require root or equivalent capability.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("requires root or CAP_NET_ADMIN; re-run with sudo")
	}
}

// --- Validation tests: no root required ---

func TestDefaultConfigIsValid(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig("test123")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig(%q).Validate() = %v, want nil", "test123", err)
	}
}

func TestDefaultConfigEnforcesIFNAMSIZ(t *testing.T) {
	t.Parallel()

	// 20-char id; without truncation "veth-" + 20 = 25 chars, over the limit.
	cfg := DefaultConfig("averylongidentifier0")
	if len(cfg.HostVeth) > 15 {
		t.Fatalf("HostVeth %q is %d chars; want <= 15 (IFNAMSIZ)",
			cfg.HostVeth, len(cfg.HostVeth))
	}
}

func TestValidateRejectsEmptyID(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig("x")
	cfg.ID = ""
	if err := cfg.Validate(); !errors.Is(err, ErrEmptyID) {
		t.Fatalf("Validate() = %v; want ErrEmptyID", err)
	}
}

func TestValidateRejectsEmptyVeth(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig("x")
	cfg.HostVeth = ""
	if err := cfg.Validate(); !errors.Is(err, ErrEmptyVeth) {
		t.Fatalf("Validate() = %v; want ErrEmptyVeth", err)
	}
}

func TestValidateRejectsLongHostVeth(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig("x")
	cfg.HostVeth = "this-name-is-way-too-long" // 25 chars
	if err := cfg.Validate(); !errors.Is(err, ErrNameTooLong) {
		t.Fatalf("Validate() = %v; want ErrNameTooLong", err)
	}
}

func TestValidateRejectsInvalidCIDR(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		hostCIDR string
		contCIDR string
	}{
		{"bad host", "not-a-cidr", "10.10.10.2/24"},
		{"bad cont", "10.10.10.1/24", "256.0.0.1/24"},
		{"missing prefix", "10.10.10.1", "10.10.10.2/24"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig("x")
			cfg.HostCIDR = tc.hostCIDR
			cfg.ContCIDR = tc.contCIDR
			if err := cfg.Validate(); !errors.Is(err, ErrInvalidCIDR) {
				t.Fatalf("Validate() = %v; want ErrInvalidCIDR", err)
			}
		})
	}
}

func TestValidateRejectsInvalidGateway(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig("x")
	cfg.Gateway = "not-an-ip"
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidGW) {
		t.Fatalf("Validate() = %v; want ErrInvalidGW", err)
	}
}

func TestParseCIDRPreservesHostBits(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cidr   string
		wantIP string
	}{
		{"10.10.10.1/24", "10.10.10.1"},
		{"192.168.5.100/16", "192.168.5.100"},
		{"172.16.0.1/12", "172.16.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.cidr, func(t *testing.T) {
			t.Parallel()
			ipnet, err := parseCIDR(tc.cidr)
			if err != nil {
				t.Fatalf("parseCIDR(%q) error = %v", tc.cidr, err)
			}
			if got := ipnet.IP.String(); got != tc.wantIP {
				t.Fatalf("IP = %s; want %s (host bits must be preserved)", got, tc.wantIP)
			}
		})
	}
}

// --- Integration tests: root required ---

func TestTeardownIsIdempotentOnAbsentLink(t *testing.T) {
	requireRoot(t)

	// A link named "veth-absent0" should not exist. Teardown must return nil.
	cfg := DefaultConfig("absent0")
	if err := Teardown(cfg); err != nil {
		t.Fatalf("Teardown on absent link: %v; want nil (idempotent)", err)
	}
}

// ExampleDefaultConfig demonstrates the naming convention enforced by DefaultConfig.
func ExampleDefaultConfig() {
	cfg := DefaultConfig("abc123")
	fmt.Printf("host=%s cont=%s\n", cfg.HostVeth, cfg.ContVeth)
	// Output:
	// host=veth-abc123 cont=eth0
}

// Your turn: add TestValidateAcceptsExactly15CharName that creates a Config
// with a 15-character HostVeth and asserts cfg.Validate() returns nil.
// Then add TestValidateRejectsLongContVeth following the same pattern.
```

Create `cmd/demo/main.go`:

```go
//go:build linux

// Demo creates a network namespace with a veth pair and verifies connectivity.
// It must run as root on a Linux host with network namespace support.
//
// Usage:
//
//	sudo go run ./cmd/demo
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"example.com/vethns"
)

func main() {
	// When re-invoked as the container process, configure and report.
	if len(os.Args) == 2 && os.Args[1] == "--container" {
		container()
		return
	}

	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "demo: must run as root (requires CAP_NET_ADMIN)")
		os.Exit(1)
	}

	cfg := vethns.DefaultConfig("demo0")

	// Build the synchronization pipe before forking.
	// Parent holds the write end; child receives the read end as fd 3.
	r, w, err := os.Pipe()
	if err != nil {
		fatal("pipe", err)
	}

	// Re-execute this binary as the container process. /proc/self/exe is the
	// current executable; passing --container switches to the child code path.
	child := exec.Command("/proc/self/exe", "--container")
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNET,
	}
	child.ExtraFiles = []*os.File{r} // r becomes fd 3 in the child
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	if err := child.Start(); err != nil {
		fatal("start child", err)
	}
	r.Close() // parent does not read from the pipe

	// Configure host-side networking. The child is blocked on fd 3 until we signal.
	if err := vethns.HostSetup(cfg, child.Process.Pid); err != nil {
		child.Process.Kill() //nolint:errcheck
		fatal("HostSetup", err)
	}
	fmt.Printf("host: %s UP with %s\n", cfg.HostVeth, cfg.HostCIDR)

	// Unblock the child by writing one byte and closing the write end.
	if _, err := w.Write([]byte{1}); err != nil {
		fatal("signal child", err)
	}
	w.Close()

	if err := child.Wait(); err != nil {
		fmt.Fprintln(os.Stderr, "child exited:", err)
	}

	// Remove the host-side veth. This also removes the container-side peer.
	if err := vethns.Teardown(cfg); err != nil {
		fatal("teardown", err)
	}
	fmt.Printf("host: %s removed\n", cfg.HostVeth)
}

// container runs inside the new network namespace (CLONE_NEWNET).
// It waits for the parent to finish HostSetup, then configures its own interfaces.
func container() {
	// fd 3 is the read end of the synchronization pipe.
	sync := os.NewFile(3, "sync-pipe")
	buf := make([]byte, 1)
	if _, err := sync.Read(buf); err != nil {
		fmt.Fprintln(os.Stderr, "container: read sync pipe:", err)
		os.Exit(1)
	}
	sync.Close()

	cfg := vethns.DefaultConfig("demo0")
	if err := vethns.ContainerSetup(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "container: ContainerSetup:", err)
		os.Exit(1)
	}

	// Show the container's network view — it must see only lo and eth0.
	fmt.Println("container: interfaces:")
	ip := exec.Command("ip", "addr", "show")
	ip.Stdout = os.Stdout
	ip.Stderr = os.Stderr
	ip.Run() //nolint:errcheck

	fmt.Printf("container: %s UP with %s gateway %s\n",
		cfg.ContVeth, cfg.ContCIDR, cfg.Gateway)
}

func fatal(label string, err error) {
	fmt.Fprintf(os.Stderr, "demo: %s: %v\n", label, err)
	os.Exit(1)
}
```

The `go.mod` for this module:

```
module example.com/vethns

go 1.26

require github.com/vishvananda/netlink v1.3.0

require (
	github.com/vishvananda/netns v0.0.5 // indirect
	golang.org/x/sys v0.29.0 // indirect
)
```

## Common Mistakes

### Not Re-Fetching the Link After LinkSetNsFd

Wrong: using the `peer` link handle after `LinkSetNsFd` returns.

```go
peer, _ := netlink.LinkByName(cfg.ContVeth)
netlink.LinkSetNsFd(peer, int(fd))
netlink.AddrAdd(peer, &netlink.Addr{IPNet: hostAddr}) // peer is stale; it has moved
```

What happens: `AddrAdd` returns ENODEV or applies the address in the wrong namespace. The `peer` object's `LinkIndex` still refers to the old namespace where the interface no longer exists.

Fix: re-fetch the host end by name after the move:

```go
host, err := netlink.LinkByName(cfg.HostVeth) // fresh lookup
netlink.AddrAdd(host, &netlink.Addr{IPNet: hostAddr})
```

### Forgetting to Bring Up Loopback

Wrong: calling `ContainerSetup` and skipping loopback configuration.

What happens: any bind or connect to `127.0.0.1` fails with `EADDRNOTAVAIL` or `ECONNREFUSED`. The kernel creates `lo` in every new namespace but leaves it DOWN. Health checks that ping localhost break silently.

Fix: always call `netlink.LinkSetUp(lo)` on the `lo` interface before any other setup.

### Assigning the Network Address Instead of the Host Address

Wrong: using `net.ParseCIDR` directly and assigning `IPNet.IP` to the interface.

```go
_, ipnet, _ := net.ParseCIDR("10.10.10.1/24")
// ipnet.IP is 10.10.10.0 (network address), not 10.10.10.1
netlink.AddrAdd(link, &netlink.Addr{IPNet: ipnet})
```

What happens: the interface gets IP `10.10.10.0/24`. Routing and ARP work against the network address, not the intended host address; pings between the two ends fail.

Fix: use the `parseCIDR` helper from this lesson, which copies `ip` (the host address) back into `IPNet.IP` before returning.

### Exceeding IFNAMSIZ Without Validation

Wrong: constructing an interface name from a UUID or long string without checking length.

```go
hostVeth := "veth-" + containerID // containerID = "a1b2c3d4-e5f6-..." (36 chars)
```

What happens: `netlink.LinkAdd` returns ENAMETOOLONG or silently truncates the name to 15 characters, causing later `LinkByName` lookups (with the full name) to fail.

Fix: enforce the 15-character limit in `Validate()` and truncate in `DefaultConfig`. Both constraints are tested.

### Starting the Child Without a Synchronization Mechanism

Wrong: starting the child, sleeping a fixed duration, then calling `HostSetup`.

```go
child.Start()
time.Sleep(500 * time.Millisecond) // "should be enough"
vethns.HostSetup(cfg, child.Process.Pid)
```

What happens: on a loaded system the sleep is too short; the child's `ContainerSetup` runs before the peer arrives in its namespace and fails. On an idle system the sleep is wasted latency.

Fix: use the pipe protocol. The child's `Read` blocks until the parent writes, regardless of load.

## Verification

This lesson requires Linux with `CAP_NET_ADMIN` (root or equivalent). The validation tests run without root; the integration tests skip automatically on non-root.

```bash
cd ~/go-exercises/vethns

# Format check (must print nothing)
test -z "$(gofmt -l .)"

# Vet (must print nothing)
go vet ./...

# Validation tests only (no root required)
go test -count=1 -race ./... -run 'TestDefault|TestValidate|TestParseCIDR'

# Full test suite including integration tests (root required)
sudo go test -count=1 -race ./...

# Demo (root required; observe host and container output)
sudo go run ./cmd/demo
```

After running the demo, verify on the host with a second terminal:

```bash
ip link show type veth   # should show no veth-demo0 after demo exits
```

If `veth-demo0` lingers, `Teardown` did not run. Manually clean up:

```bash
sudo ip link delete veth-demo0
```

## Summary

- `CLONE_NEWNET` gives a process its own network stack; the new namespace starts with only a DOWN loopback, nothing else.
- A veth pair is two virtual interfaces joined atomically; packets sent out one end arrive at the other.
- `netlink.LinkSetNsFd` moves a veth end into a foreign namespace identified by an open `/proc/<pid>/ns/net` fd; the link disappears from the current namespace immediately.
- Deleting either end of a veth pair removes both; cleanup requires one `netlink.LinkDel` call.
- The synchronization pipe is mandatory: without it the child calls `ContainerSetup` before `eth0` exists.
- IFNAMSIZ is 15 characters (16 with null); enforce and test this in any scheme that derives names from user-supplied IDs.
- Goroutine-namespace interaction is thread-local; use `runtime.LockOSThread` or run configuration from the target process itself.

## What's Next

Next: [Cgroups v2: CPU and Memory Limits](../04-cgroups-v2-cpu-memory/04-cgroups-v2-cpu-memory.md).

## Resources

- [network_namespaces(7) man page](https://man7.org/linux/man-pages/man7/network_namespaces.7.html) — semantics, lifecycle, and fd-based reference counting
- [veth(4) man page](https://man7.org/linux/man-pages/man4/veth.4.html) — virtual Ethernet pair creation and behavior
- [vishvananda/netlink — Go netlink library](https://github.com/vishvananda/netlink) — source of all API signatures used in this lesson
- [pkg.go.dev/net#ParseCIDR](https://pkg.go.dev/net#ParseCIDR) — documents the network-address behavior that parseCIDR corrects
- [Runc network namespace setup (source)](https://github.com/opencontainers/runc/blob/main/libcontainer/configs/network.go) — production reference for how OCI runtimes configure container networking
