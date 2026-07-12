# Exercise 8: An Exact-Match IP Allowlist Using netip.Addr and [16]byte Keys

An access-control allowlist at the request edge needs an O(1) exact-match check on
an IP address. `net/netip.Addr` is a comparable value that wraps fixed byte arrays,
so it can be a map key directly — and normalizing to a `[16]byte` via `As16()`
gives a uniform key across IPv4 and IPv6. This exercise builds that allowlist and
pins the `As4`/`As16` panic condition.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
ipallow/                     independent module: example.com/ipallow
  go.mod
  allow.go                   Allowlist{map[[16]byte]struct{}}; Add, Allowed; key via Addr.As16
  cmd/
    demo/
      main.go                runnable demo: add v4/v6, check allowed/denied
  allow_test.go              v4+v6 membership, AddrFrom4 As16 stability, zero addr denied, As4-panic
```

- Files: `allow.go`, `cmd/demo/main.go`, `allow_test.go`.
- Implement: an `Allowlist` over `map[[16]byte]struct{}`, with `Add(netip.Addr)` and `Allowed(netip.Addr) bool`, keying on the address's `As16()` array.
- Test: IPv4 and IPv6 membership; an `AddrFrom4` address and its `As16()` key are stable; the zero `netip.Addr` is never allowed; calling `As4` on an IPv6 address panics (documented, recover-based).
- Verify: `go test -count=1 -race ./...`

### Why normalize to [16]byte via As16

`netip.Addr` is itself comparable — you *can* use `map[netip.Addr]struct{}`
directly, and that is often the cleanest choice. But `netip.Addr` carries more than
the raw bytes: an IPv4 address and its IPv4-in-IPv6 form (`::ffff:a.b.c.d`) are
distinct `Addr` values even though they denote the same host, and an `Addr` also
carries an optional zone for link-local IPv6. Normalizing every address to its
16-byte representation with `Addr.As16()` collapses those distinctions to one
canonical key: `As16()` returns the address as a `[16]byte`, mapping an IPv4
address into the IPv4-in-IPv6 range so that a v4 address has a single, stable
16-byte form. That `[16]byte` is comparable, so it keys the map with no allocation.

The panic conditions are the trap to internalize. `Addr.As4()` returns a `[4]byte`
but **panics** if the address is not a 4-byte IPv4 address — including any IPv6
address and the zero `Addr`. `As16()` is the safe universal choice: it is defined
for every address (v4, v6, and even the zero value, which yields all-zero bytes).
So the allowlist keys on `As16()` unconditionally and never risks the `As4` panic.
The test deliberately demonstrates the panic with `recover` to justify why the code
avoids `As4`.

The zero-address guard matters for correctness: a request with no valid source
address parses to the zero `netip.Addr`, whose `As16()` is all zeros. Unless you
explicitly `Add` the zero address (you never should), it is not in the map and
`Allowed` returns false — a safe default. `AddrFrom4([4]byte)` is the constructor
the tests use to build IPv4 addresses without parsing, showing the array-to-`Addr`
direction: four raw bytes in, a comparable `Addr` out, whose `As16()` is a stable
16-byte key.

Create `allow.go`:

```go
package ipallow

import (
	"net/netip"
)

// Allowlist is an exact-match set of permitted IP addresses. It keys on the
// 16-byte canonical form of each address, which is uniform across IPv4 and IPv6.
type Allowlist struct {
	set map[[16]byte]struct{}
}

// New returns an empty Allowlist.
func New() *Allowlist {
	return &Allowlist{set: make(map[[16]byte]struct{})}
}

// Add permits addr. Invalid (zero) addresses are ignored so they can never be
// allowed by accident.
func (a *Allowlist) Add(addr netip.Addr) {
	if !addr.IsValid() {
		return
	}
	a.set[addr.As16()] = struct{}{}
}

// Allowed reports whether addr is on the list. As16 is defined for every address,
// so this never panics the way As4 would on an IPv6 or zero address.
func (a *Allowlist) Allowed(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	_, ok := a.set[addr.As16()]
	return ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/netip"

	"example.com/ipallow"
)

func main() {
	a := ipallow.New()
	a.Add(netip.MustParseAddr("10.0.0.1"))
	a.Add(netip.MustParseAddr("2001:db8::1"))

	checks := []string{"10.0.0.1", "10.0.0.2", "2001:db8::1", "::1"}
	for _, s := range checks {
		addr := netip.MustParseAddr(s)
		fmt.Printf("%-12s allowed=%v\n", s, a.Allowed(addr))
	}

	// The zero address is never allowed.
	fmt.Printf("zero addr allowed=%v\n", a.Allowed(netip.Addr{}))

	// An IPv4 built from raw bytes shares the key with its parsed form.
	fromBytes := netip.AddrFrom4([4]byte{10, 0, 0, 1})
	parsed := netip.MustParseAddr("10.0.0.1")
	fmt.Printf("AddrFrom4 key stable: %v\n", fromBytes.As16() == parsed.As16())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
10.0.0.1     allowed=true
10.0.0.2     allowed=false
2001:db8::1  allowed=true
::1          allowed=false
zero addr allowed=false
AddrFrom4 key stable: true
```

### Tests

`TestMembership` adds IPv4 and IPv6 addresses and asserts listed ones are allowed
and unlisted ones are not. `TestAddrFrom4KeyStable` builds an IPv4 via `AddrFrom4`
and asserts its `As16()` key equals that of the parsed form and is comparable.
`TestZeroAddrDenied` asserts the zero `netip.Addr` is never allowed.
`TestAs4PanicsOnIPv6` documents, with `recover`, that `As4` panics on an IPv6
address — the reason the allowlist keys on `As16` instead.

Create `allow_test.go`:

```go
package ipallow

import (
	"net/netip"
	"testing"
)

func TestMembership(t *testing.T) {
	t.Parallel()

	a := New()
	a.Add(netip.MustParseAddr("192.168.1.10"))
	a.Add(netip.MustParseAddr("2001:db8::abcd"))

	allowed := []string{"192.168.1.10", "2001:db8::abcd"}
	for _, s := range allowed {
		if !a.Allowed(netip.MustParseAddr(s)) {
			t.Fatalf("%s should be allowed", s)
		}
	}
	denied := []string{"192.168.1.11", "2001:db8::1", "10.0.0.1"}
	for _, s := range denied {
		if a.Allowed(netip.MustParseAddr(s)) {
			t.Fatalf("%s should be denied", s)
		}
	}
}

func TestAddrFrom4KeyStable(t *testing.T) {
	t.Parallel()

	fromBytes := netip.AddrFrom4([4]byte{203, 0, 113, 5})
	parsed := netip.MustParseAddr("203.0.113.5")

	if fromBytes.As16() != parsed.As16() {
		t.Fatal("AddrFrom4 and parsed IPv4 must share the same 16-byte key")
	}

	a := New()
	a.Add(fromBytes)
	if !a.Allowed(parsed) {
		t.Fatal("address added via AddrFrom4 should be allowed when checked as parsed")
	}
}

func TestZeroAddrDenied(t *testing.T) {
	t.Parallel()

	a := New()
	a.Add(netip.Addr{}) // ignored
	if a.Allowed(netip.Addr{}) {
		t.Fatal("the zero address must never be allowed")
	}
}

func TestAs4PanicsOnIPv6(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("As4 on an IPv6 address should panic")
		}
	}()
	v6 := netip.MustParseAddr("2001:db8::1")
	_ = v6.As4() // panics: not a 4-byte address
}
```

## Review

The allowlist is correct when membership is an exact match keyed on the canonical
16-byte address form, so IPv4 and IPv6 share one map type and unlisted addresses —
including the zero address — are denied. Keying on `As16()` rather than `As4()` is
the load-bearing choice: `As16` is total (defined for every address), while `As4`
panics on IPv6 and the zero value, as `TestAs4PanicsOnIPv6` demonstrates.
`netip.Addr` being a comparable value wrapping fixed arrays is what makes both the
direct `map[netip.Addr]` and the normalized `map[[16]byte]` approaches sound; this
exercise takes the normalized route to collapse v4/v6 and zone distinctions. Run
`go test -race` to confirm membership, the `AddrFrom4` key stability, and the
zero-address denial.

## Resources

- [net/netip](https://pkg.go.dev/net/netip) — `Addr` as a comparable value; `ParseAddr`, `AddrFrom4`, `As16`, `As4`, `IsValid`.
- [netip.Addr.As4](https://pkg.go.dev/net/netip#Addr.As4) — documents the panic when the address is not 4-byte IPv4.
- [netip.Addr.As16](https://pkg.go.dev/net/netip#Addr.As16) — the total 16-byte form used as the map key.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-l2-device-table-mac-key.md](07-l2-device-table-mac-key.md) | Next: [09-fixed-window-rate-limiter.md](09-fixed-window-rate-limiter.md)
