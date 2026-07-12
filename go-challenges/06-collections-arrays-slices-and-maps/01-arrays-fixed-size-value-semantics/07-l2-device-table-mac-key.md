# Exercise 7: An L2 Device/Forwarding Table Keyed by [6]byte MAC Address

A switch's forwarding table maps a MAC address to the port it was last seen on. A
MAC is exactly six bytes, so `[6]byte` is the natural key type: comparable, used
directly as a `map[[6]byte]Port` key with no string allocation per lookup. This
exercise builds that table and a `ParseMAC` that decodes `aa:bb:cc:dd:ee:ff` into a
fixed `[6]byte`.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
l2table/                     independent module: example.com/l2table
  go.mod
  table.go                   type MAC [6]byte; ParseMAC; DeviceTable{map[MAC]Port}; Learn/Lookup
  cmd/
    demo/
      main.go                runnable demo: parse, learn on a port, look up, format
  table_test.go              parse round-trip, reject malformed, learn/lookup, interning ==
```

- Files: `table.go`, `cmd/demo/main.go`, `table_test.go`.
- Implement: `MAC [6]byte`, `ParseMAC(s string) (MAC, error)`, `(MAC).String()`, and a `DeviceTable` over `map[MAC]Port` with `Learn` and `Lookup`.
- Test: parse a valid MAC and round-trip its formatting; reject malformed/short input; learn on a port then look it up; two equal parsed MACs are `==` and collide to one entry.
- Verify: `go test -count=1 -race ./...`

### Why the [6]byte MAC is the key directly

An IEEE 802 MAC address is six octets. Modeling it as `MAC [6]byte` welds that size
into the type, and — because a fixed-size array of bytes is comparable — lets the
table key on the MAC value directly: `map[MAC]Port`. There is no
`hex.EncodeToString` or `mac.String()` per operation, no intermediate string
allocation, and no risk of two different string encodings of the same address
missing each other. The six bytes ARE the key.

`ParseMAC` uses `net.ParseMAC`, the standard parser, which accepts the common
`aa:bb:cc:dd:ee:ff` form (and several others) and returns a `net.HardwareAddr` —
which is a `[]byte`. A 48-bit MAC decodes to exactly six bytes, so `ParseMAC`
length-checks (`len(hw) != 6`) and then copies the six bytes into the array with
`copy(m[:], hw)`. The length check matters because `net.ParseMAC` also parses
EUI-64 (8-byte) and 20-byte InfiniBand addresses; a device table that expects a
48-bit MAC must reject anything longer rather than truncating it. This is the same
guarded slice-to-array copy pattern the concepts file describes, done with `copy`
so it never panics on a short input — it just copies `min(len(dst), len(src))`.

The "interning" behavior falls out of value semantics: parse the same MAC string
twice and you get two `MAC` values that are `==`, so they hash to the same bucket
and a `Learn` followed by a `Lookup` of an independently-parsed equal MAC finds the
entry. Fixed-size arrays are first-class comparable identifiers; two equal ones are
interchangeable everywhere, including as map keys.

Create `table.go`:

```go
package l2table

import (
	"fmt"
	"net"
)

// MAC is a 48-bit hardware address. The [6]byte size is part of the type, and the
// array is comparable so it can key a forwarding table directly.
type MAC [6]byte

// Port identifies a switch port.
type Port int

// ParseMAC decodes a textual MAC (e.g. "aa:bb:cc:dd:ee:ff") into a fixed [6]byte.
// It rejects any address that is not exactly 6 bytes (EUI-64 and longer forms).
func ParseMAC(s string) (MAC, error) {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return MAC{}, err
	}
	if len(hw) != 6 {
		return MAC{}, fmt.Errorf("l2table: %q is %d bytes, want a 6-byte MAC", s, len(hw))
	}
	var m MAC
	copy(m[:], hw)
	return m, nil
}

// String formats the MAC in the canonical colon-separated lowercase hex form.
func (m MAC) String() string {
	return net.HardwareAddr(m[:]).String()
}

// DeviceTable is a switch forwarding table mapping a learned MAC to a port.
type DeviceTable struct {
	entries map[MAC]Port
}

// NewDeviceTable returns an empty forwarding table.
func NewDeviceTable() *DeviceTable {
	return &DeviceTable{entries: make(map[MAC]Port)}
}

// Learn records that mac was last seen on port.
func (d *DeviceTable) Learn(mac MAC, port Port) {
	d.entries[mac] = port
}

// Lookup returns the port a MAC was learned on, if known.
func (d *DeviceTable) Lookup(mac MAC) (Port, bool) {
	p, ok := d.entries[mac]
	return p, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/l2table"
)

func main() {
	tbl := l2table.NewDeviceTable()

	mac, err := l2table.ParseMAC("aa:bb:cc:dd:ee:ff")
	if err != nil {
		panic(err)
	}
	tbl.Learn(mac, 3)

	// A second, independent parse of the same address.
	same, _ := l2table.ParseMAC("aa:bb:cc:dd:ee:ff")
	fmt.Printf("interned equal: %v\n", mac == same)

	if port, ok := tbl.Lookup(same); ok {
		fmt.Printf("lookup %s -> port %d\n", same, port)
	}

	other, _ := l2table.ParseMAC("11:22:33:44:55:66")
	_, ok := tbl.Lookup(other)
	fmt.Printf("unknown MAC found: %v\n", ok)

	if _, err := l2table.ParseMAC("not-a-mac"); err != nil {
		fmt.Println("rejected malformed input")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
interned equal: true
lookup aa:bb:cc:dd:ee:ff -> port 3
unknown MAC found: false
rejected malformed input
```

### Tests

`TestParseRoundTrip` parses a MAC and asserts `String()` returns the canonical form.
`TestParseRejectsBad` is a table of malformed and wrong-length inputs, each expected
to error. `TestLearnLookup` learns a MAC on a port, looks it up, and asserts a
different MAC misses. `TestInterning` parses the same string twice and asserts the
two `MAC` values are `==` and collide to a single map entry.

Create `table_test.go`:

```go
package l2table

import (
	"testing"
)

func TestParseRoundTrip(t *testing.T) {
	t.Parallel()

	m, err := ParseMAC("aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	if got := m.String(); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("String() = %q, want aa:bb:cc:dd:ee:ff", got)
	}
}

func TestParseRejectsBad(t *testing.T) {
	t.Parallel()

	bad := []string{
		"",
		"not-a-mac",
		"aa:bb:cc:dd:ee",          // too short
		"aa:bb:cc:dd:ee:ff:00:11", // EUI-64, 8 bytes
		"zz:bb:cc:dd:ee:ff",       // non-hex
	}
	for _, s := range bad {
		if _, err := ParseMAC(s); err == nil {
			t.Fatalf("ParseMAC(%q) should have failed", s)
		}
	}
}

func TestLearnLookup(t *testing.T) {
	t.Parallel()

	tbl := NewDeviceTable()
	mac, _ := ParseMAC("aa:bb:cc:dd:ee:ff")
	tbl.Learn(mac, 7)

	if p, ok := tbl.Lookup(mac); !ok || p != 7 {
		t.Fatalf("Lookup = %d,%v; want 7,true", p, ok)
	}

	other, _ := ParseMAC("11:22:33:44:55:66")
	if _, ok := tbl.Lookup(other); ok {
		t.Fatal("unlearned MAC should miss")
	}
}

func TestInterning(t *testing.T) {
	t.Parallel()

	a, _ := ParseMAC("de:ad:be:ef:00:01")
	b, _ := ParseMAC("de:ad:be:ef:00:01")
	if a != b {
		t.Fatal("two parses of the same MAC must be ==")
	}

	tbl := NewDeviceTable()
	tbl.Learn(a, 1)
	tbl.Learn(b, 2) // same key: overwrites, does not add
	if got := len(tbl.entries); got != 1 {
		t.Fatalf("equal MACs must collide to one entry, got %d", got)
	}
	if p, _ := tbl.Lookup(a); p != 2 {
		t.Fatalf("second Learn should overwrite: port = %d, want 2", p)
	}
}
```

## Review

The table is correct when the `[6]byte` MAC keys the map directly and two equal
MACs are interchangeable: `TestInterning` proves independently-parsed equal MACs
are `==` and collapse to one entry. `ParseMAC` is correct when it accepts the
canonical 48-bit form, round-trips through `String()`, and rejects anything that is
not exactly six bytes — the length guard after `net.ParseMAC` is essential because
that parser also returns 8- and 20-byte addresses. The array-as-identifier pattern
here is the same one behind the content-addressed store and the IP allowlist: a
fixed-size comparable array is a first-class key with no string encoding. Run
`go test -race` to confirm parse rejection, learn/lookup, and the interning
collision.

## Resources

- [net.ParseMAC](https://pkg.go.dev/net#ParseMAC) — parses MAC-48, EUI-48, EUI-64, and longer forms into a `HardwareAddr`.
- [net.HardwareAddr](https://pkg.go.dev/net#HardwareAddr) — the `[]byte` MAC type and its canonical `String()`.
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — comparable key requirement satisfied by `[6]byte`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-gcm-nonce-and-tag-verify.md](06-gcm-nonce-and-tag-verify.md) | Next: [08-ip-allowlist-netip-fixed-array.md](08-ip-allowlist-netip-fixed-array.md)
