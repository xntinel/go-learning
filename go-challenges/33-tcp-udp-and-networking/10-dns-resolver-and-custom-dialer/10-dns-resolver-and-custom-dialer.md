# 10. DNS Resolver and Custom Dialer

Go exposes two concrete types for controlling how connections are established: `net.Resolver` handles DNS lookups and `net.Dialer` handles TCP/UDP connection opening. In production systems you need to redirect internal hostnames without touching `/etc/hosts`, query a corporate or anycast nameserver instead of the system default, or log every TCP connection before it completes. The hard part is knowing which layer to change: `net.Dialer.Resolver` only replaces the nameserver; intercepting individual hostname-to-IP mappings requires a custom `DialContext` that consults an in-memory table before falling through to real DNS.

```text
dnsdialer/
  go.mod
  dnsdialer.go
  dnsdialer_test.go
  cmd/demo/main.go
```

## Concepts

### The Two Layers of a Dial

When `http.Client` needs `api.example.com:443`, the sequence is:

1. `http.Transport.DialContext` is called with `"tcp"` and `"api.example.com:443"`.
2. `net.Dialer` resolves `"api.example.com"` via its `Resolver` field.
3. `net.Dialer.Resolver.Dial` opens a UDP socket to the nameserver and receives A/AAAA records.
4. `net.Dialer` opens a TCP socket to the resolved IP.

You can hook at each of these three points. Replacing `http.Transport.DialContext` gives the most control: you drive both steps 2 and 4 yourself. Replacing `net.Dialer.Resolver` only changes which nameserver is queried in step 3 — you still let the standard library resolve the hostname.

### net.Resolver: Changing the Nameserver

```go
type Resolver struct {
	PreferGo     bool
	StrictErrors bool
	Dial         func(ctx context.Context, network, address string) (net.Conn, error)
}
```

`PreferGo: true` forces the pure-Go DNS client, bypassing CGO and `/etc/nsswitch.conf`. The `Dial` field replaces the function used to open a socket to the nameserver. The `address` argument that Go passes to `Dial` is the default nameserver from `/etc/resolv.conf`; you are free to ignore it and hard-code a target:

```go
r := &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		return d.DialContext(ctx, "udp", "8.8.8.8:53")
	},
}
```

Plug `r` into `net.Dialer.Resolver` and every hostname resolved by that dialer goes through `8.8.8.8:53`.

### CGO vs Pure-Go Resolver

Go selects the resolver at startup based on `/etc/nsswitch.conf`, the build environment, and `GODEBUG`. Setting `GODEBUG=netdns=go` forces the pure-Go resolver; `GODEBUG=netdns=cgo` forces CGO. The pure-Go resolver is faster for short-lived processes (no CGO handoff), honours `context.Context` cancellation on individual DNS requests, and is always used when `PreferGo: true` on a per-resolver basis. The CGO resolver honours system name-service plugins (LDAP, mDNS, SSSD), which the pure-Go resolver ignores. See [Name Resolution in Go](https://pkg.go.dev/net#hdr-Name_Resolution).

### Hostname Overrides via a Custom DialContext

`net.Dialer.Resolver` is `*net.Resolver` — a concrete type, not an interface. You cannot inject an in-memory hostname table through it. Instead, replace `http.Transport.DialContext` with a closure that:

1. Splits the address into host and port with `net.SplitHostPort`.
2. Looks the host up in an override table.
3. If found, dials the IP directly with the real port.
4. If not found, falls through to a real `Resolver`.

This is the pattern used by service-mesh sidecars, integration-test fixtures (redirect `external-api.com` to a local mock server), and client-side load balancers that maintain a live endpoint table from service discovery.

### net.Dialer Fields

Key fields used in this lesson:

- `Timeout` — caps the entire dial (DNS resolution plus TCP connect).
- `KeepAlive` — sets the TCP keep-alive probe interval (negative disables it; zero uses the OS default of ~15 s).
- `Resolver` — the `*net.Resolver` for hostname lookups.
- `ControlContext` — called with the raw socket (`syscall.RawConn`) before `connect(2)`, for setting socket options such as `SO_MARK` or `TCP_USER_TIMEOUT`.

## Exercises

### Exercise 1: The Resolver Interface and OverrideMap

Define a minimal `Resolver` interface. `*net.Resolver` already satisfies it, which lets tests inject a lightweight mock without network I/O.

Create `dnsdialer.go`:

```go
package dnsdialer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

var (
	ErrEmptyHost = errors.New("dnsdialer: host must not be empty")
	ErrNoAddrs   = errors.New("dnsdialer: no addresses resolved")
)

// Resolver resolves a hostname to a list of IP strings.
// *net.Resolver satisfies this interface.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// OverrideMap resolves known hostnames from an in-memory table and delegates
// everything else to a fallback Resolver. Once constructed with NewOverrideMap
// an OverrideMap is safe for concurrent reads.
type OverrideMap struct {
	entries  map[string][]string
	fallback Resolver
}

// NewOverrideMap creates an OverrideMap. entries maps hostnames to slices of
// IP address strings (e.g. {"api.internal": {"10.0.0.1"}}). If fallback is
// nil, net.DefaultResolver is used.
func NewOverrideMap(entries map[string][]string, fallback Resolver) *OverrideMap {
	e := make(map[string][]string, len(entries))
	for k, v := range entries {
		e[k] = append([]string(nil), v...)
	}
	fb := fallback
	if fb == nil {
		fb = net.DefaultResolver
	}
	return &OverrideMap{entries: e, fallback: fb}
}

// LookupHost returns override IPs for host if an entry exists; otherwise it
// delegates to the fallback resolver.
func (o *OverrideMap) LookupHost(ctx context.Context, host string) ([]string, error) {
	if host == "" {
		return nil, ErrEmptyHost
	}
	if ips, ok := o.entries[host]; ok {
		return append([]string(nil), ips...), nil
	}
	return o.fallback.LookupHost(ctx, host)
}

// Entries returns a snapshot copy of the override table.
func (o *OverrideMap) Entries() map[string][]string {
	out := make(map[string][]string, len(o.entries))
	for k, v := range o.entries {
		out[k] = append([]string(nil), v...)
	}
	return out
}
```

`NewOverrideMap` copies the caller's slice contents so mutations to the original map after construction do not affect the `OverrideMap`.

### Exercise 2: Custom DNS Server Resolver and Dialer

`NewPublicDNSResolver` ignores the `address` argument passed by the standard library (which comes from `/etc/resolv.conf`) and always connects to the nameserver you supply.

Append to `dnsdialer.go`:

```go
// NewPublicDNSResolver returns a *net.Resolver configured to query nameserver
// (e.g. "8.8.8.8:53") using the pure-Go DNS implementation. Setting
// PreferGo: true bypasses CGO and /etc/nsswitch.conf.
func NewPublicDNSResolver(nameserver string, dialTimeout time.Duration) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: dialTimeout}
			return d.DialContext(ctx, "udp", nameserver)
		},
	}
}

// NewDialer returns a *net.Dialer whose Resolver field is r.
// net.Dialer.Resolver is *net.Resolver (a concrete type, not an interface),
// so DNS overrides must go through a custom DialContext instead — see
// NewOverrideDialer.
func NewDialer(r *net.Resolver, timeout, keepAlive time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout:   timeout,
		KeepAlive: keepAlive,
		Resolver:  r,
	}
}
```

### Exercise 3: Override Dialer and HTTP Integration

`NewOverrideDialer` wraps an `OverrideMap` in a closure whose type matches `http.Transport.DialContext` exactly. The closure splits the address, resolves the host through the map (falling through to real DNS for unknown hosts), and dials the raw IP.

Append to `dnsdialer.go`:

```go
// NewOverrideDialer returns a DialContext function that resolves hostnames
// through om before opening connections. Its signature matches
// http.Transport.DialContext.
func NewOverrideDialer(om *OverrideMap, timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	inner := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("dnsdialer: split %q: %w", addr, err)
		}
		ips, err := om.LookupHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("dnsdialer: lookup %q: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%w for %q", ErrNoAddrs, host)
		}
		var lastErr error
		for _, ip := range ips {
			c, dialErr := inner.DialContext(ctx, network, net.JoinHostPort(ip, port))
			if dialErr == nil {
				return c, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

// NewHTTPClient builds an *http.Client whose transport routes all connections
// through dialCtx. Pass (*net.Dialer).DialContext or NewOverrideDialer.
func NewHTTPClient(dialCtx func(context.Context, string, string) (net.Conn, error)) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: dialCtx,
		},
	}
}
```

The demo program exercises all exported API from `package main`:

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"example.com/dnsdialer"
)

func main() {
	om := dnsdialer.NewOverrideMap(map[string][]string{
		"api.internal": {"192.168.1.100"},
		"db.internal":  {"192.168.1.200", "192.168.1.201"},
	}, nil)

	ctx := context.Background()
	ips, err := om.LookupHost(ctx, "api.internal")
	if err != nil {
		log.Fatalf("lookup api.internal: %v", err)
	}
	fmt.Printf("api.internal -> %v\n", ips)
	fmt.Printf("override table has %d entries\n", len(om.Entries()))

	resolver := dnsdialer.NewPublicDNSResolver("8.8.8.8:53", 5*time.Second)
	dialer := dnsdialer.NewDialer(resolver, 10*time.Second, 30*time.Second)
	_ = dnsdialer.NewHTTPClient(dialer.DialContext)
	fmt.Println("HTTP client with custom DNS resolver: ready")

	_ = dnsdialer.NewHTTPClient(dnsdialer.NewOverrideDialer(om, 10*time.Second))
	fmt.Println("HTTP client with DNS override map: ready")
}
```

Run with `go run ./cmd/demo`.

### Exercise 4: Tests

Create `dnsdialer_test.go`:

```go
package dnsdialer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// -- OverrideMap unit tests (no network I/O) ----------------------------------

func TestOverrideMapReturnsConfiguredIPs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		host string
		want []string
	}{
		{name: "single ip", host: "api.internal", want: []string{"10.0.0.1"}},
		{name: "multiple ips", host: "db.internal", want: []string{"10.0.0.2", "10.0.0.3"}},
	}
	om := NewOverrideMap(map[string][]string{
		"api.internal": {"10.0.0.1"},
		"db.internal":  {"10.0.0.2", "10.0.0.3"},
	}, nil)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := om.LookupHost(context.Background(), tc.host)
			if err != nil {
				t.Fatalf("LookupHost(%q) err = %v", tc.host, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("LookupHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
			for i, ip := range got {
				if ip != tc.want[i] {
					t.Errorf("LookupHost(%q)[%d] = %q, want %q", tc.host, i, ip, tc.want[i])
				}
			}
		})
	}
}

func TestOverrideMapRejectsEmptyHost(t *testing.T) {
	t.Parallel()

	om := NewOverrideMap(nil, nil)
	_, err := om.LookupHost(context.Background(), "")
	if !errors.Is(err, ErrEmptyHost) {
		t.Fatalf("err = %v, want ErrEmptyHost", err)
	}
}

func TestOverrideMapCopiesInputEntries(t *testing.T) {
	t.Parallel()

	original := map[string][]string{"h": {"1.2.3.4"}}
	om := NewOverrideMap(original, nil)
	original["h"] = []string{"5.6.7.8"} // mutate after construction

	ips, err := om.LookupHost(context.Background(), "h")
	if err != nil {
		t.Fatal(err)
	}
	if ips[0] != "1.2.3.4" {
		t.Fatalf("ips[0] = %q, want 1.2.3.4 (NewOverrideMap must copy, not alias)", ips[0])
	}
}

func TestOverrideMapEntriesReturnsCopy(t *testing.T) {
	t.Parallel()

	om := NewOverrideMap(map[string][]string{"h": {"1.2.3.4"}}, nil)
	snap := om.Entries()
	snap["h"] = []string{"mutated"}

	got, _ := om.LookupHost(context.Background(), "h")
	if got[0] != "1.2.3.4" {
		t.Fatalf("Entries snapshot mutation leaked into OverrideMap")
	}
}

func TestOverrideMapFallbackForUnknownHost(t *testing.T) {
	t.Parallel()

	mock := &mockResolver{addrs: []string{"192.0.2.1"}}
	om := NewOverrideMap(nil, mock)

	got, err := om.LookupHost(context.Background(), "unknown.host")
	if err != nil {
		t.Fatalf("LookupHost err = %v", err)
	}
	if len(got) != 1 || got[0] != "192.0.2.1" {
		t.Fatalf("got = %v, want [192.0.2.1]", got)
	}
}

// -- constructor tests --------------------------------------------------------

func TestNewPublicDNSResolverPreferGo(t *testing.T) {
	t.Parallel()

	r := NewPublicDNSResolver("8.8.8.8:53", 5*time.Second)
	if r == nil {
		t.Fatal("NewPublicDNSResolver returned nil")
	}
	if !r.PreferGo {
		t.Fatal("PreferGo must be true")
	}
}

func TestNewDialerFields(t *testing.T) {
	t.Parallel()

	d := NewDialer(net.DefaultResolver, 10*time.Second, 30*time.Second)
	if d.Timeout != 10*time.Second {
		t.Fatalf("Timeout = %v, want 10s", d.Timeout)
	}
	if d.KeepAlive != 30*time.Second {
		t.Fatalf("KeepAlive = %v, want 30s", d.KeepAlive)
	}
	if d.Resolver != net.DefaultResolver {
		t.Fatal("Resolver not set")
	}
}

// -- integration tests (loopback only) ----------------------------------------

func TestOverrideDialerConnectsViaOverride(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	_, port, _ := net.SplitHostPort(ln.Addr().String())

	om := NewOverrideMap(map[string][]string{
		"fake.internal": {"127.0.0.1"},
	}, nil)

	accepted := make(chan error, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			accepted <- aerr
			return
		}
		c.Close()
		accepted <- nil
	}()

	dialCtx := NewOverrideDialer(om, 5*time.Second)
	conn, err := dialCtx(context.Background(), "tcp", "fake.internal:"+port)
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	conn.Close()

	if aerr := <-accepted; aerr != nil {
		t.Fatalf("accept error: %v", aerr)
	}
}

func TestOverrideDialerUsesFallbackForUnknownHost(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	_, port, _ := net.SplitHostPort(ln.Addr().String())

	mock := &mockResolver{addrs: []string{"127.0.0.1"}}
	om := NewOverrideMap(nil, mock)

	accepted := make(chan error, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			accepted <- aerr
			return
		}
		c.Close()
		accepted <- nil
	}()

	dialCtx := NewOverrideDialer(om, 5*time.Second)
	conn, err := dialCtx(context.Background(), "tcp", "any.host:"+port)
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	conn.Close()

	if aerr := <-accepted; aerr != nil {
		t.Fatalf("accept error: %v", aerr)
	}
}

// mockResolver implements Resolver and always returns fixed addrs.
type mockResolver struct {
	addrs []string
}

func (m *mockResolver) LookupHost(_ context.Context, _ string) ([]string, error) {
	return append([]string(nil), m.addrs...), nil
}

// ExampleNewOverrideMap is auto-verified by go test.
func ExampleNewOverrideMap() {
	om := NewOverrideMap(map[string][]string{
		"api.internal": {"10.0.0.1", "10.0.0.2"},
	}, nil)
	ips, _ := om.LookupHost(context.Background(), "api.internal")
	fmt.Println(ips)
	// Output: [10.0.0.1 10.0.0.2]
}
```

Your turn: add `TestOverrideDialerReturnsErrEmptyHost` — call `NewOverrideDialer` with an `OverrideMap` that has no entries and a nil fallback (use a `mockResolver` that returns an error), dial a host with an empty hostname component, and assert the error wraps `ErrEmptyHost`.

## Common Mistakes

### Assuming net.Dialer.Resolver Supports Per-Host Overrides

Wrong: setting a `*net.Resolver` whose `Dial` function switches DNS servers based on the hostname being looked up. `Dial` is called to open a socket to the nameserver — it receives the nameserver address, not the hostname being resolved.

Fix: use `NewOverrideDialer` and keep the override table in `OverrideMap`. The override happens at the DialContext layer, before DNS is consulted.

### Ignoring address in net.Resolver.Dial and Using the Wrong Port

Wrong:
```go
Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
	d := net.Dialer{}
	return d.DialContext(ctx, "udp", "8.8.8.8") // missing :53
},
```

`address` is the default nameserver from `/etc/resolv.conf` including its port. When you hard-code a custom nameserver you must supply the port explicitly — `"8.8.8.8:53"`. Without it, `DialContext` returns an error and DNS lookups fail silently.

Fix: always include the port: `"8.8.8.8:53"`.

### Mutating the OverrideMap After Construction

Wrong: appending to the `entries` map returned by `Entries()` and expecting the `OverrideMap` to see the change. `Entries()` returns a copy for precisely this reason.

Fix: construct a new `OverrideMap` with the updated entries. `OverrideMap` is intentionally read-only after construction to be safe for concurrent use.

### Using go run instead of go test as the Verification Mechanism

Wrong: writing a `main()` that prints resolved IPs and eyeballing the output. If `LookupHost` returns the wrong slice, the print still succeeds and the regression is invisible.

Fix: use `go test -race -count=1 ./...`. The `TestOverrideMapReturnsConfiguredIPs` table drives every hostname and compares results without human involvement.

## Verification

From `~/go-exercises/dnsdialer`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The loopback integration tests require no internet access. After they pass, run the demo:

```bash
go run ./cmd/demo
```

## Summary

- `net.Resolver` customizes which nameserver is queried; `PreferGo: true` uses the pure-Go DNS client and honours `context.Context` cancellation.
- `net.Dialer.Resolver` is `*net.Resolver` — a concrete type. Per-hostname overrides require a custom `DialContext`, not a custom `Resolver`.
- `OverrideMap` resolves known hostnames from an in-memory table and delegates everything else to a fallback `Resolver`. The `Resolver` interface is satisfied by `*net.Resolver`, enabling real or mock fallbacks without code changes.
- `NewOverrideDialer` wraps an `OverrideMap` in a closure whose type matches `http.Transport.DialContext`, making it a drop-in for any HTTP client.
- Keeping `OverrideMap` immutable after construction lets it be shared across goroutines without locks.

## What's Next

Next: [HTTP Keep-Alive Analysis](../11-http-keep-alive-analysis/11-http-keep-alive-analysis.md).

## Resources

- [net.Resolver — pkg.go.dev](https://pkg.go.dev/net#Resolver)
- [net.Dialer — pkg.go.dev](https://pkg.go.dev/net#Dialer)
- [Name Resolution in Go — pkg.go.dev](https://pkg.go.dev/net#hdr-Name_Resolution)
- [net.SplitHostPort — pkg.go.dev](https://pkg.go.dev/net#SplitHostPort)
- [http.Transport.DialContext — pkg.go.dev](https://pkg.go.dev/net/http#Transport)
