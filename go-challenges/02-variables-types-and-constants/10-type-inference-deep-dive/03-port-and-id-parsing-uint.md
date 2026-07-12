# Exercise 3: Parse A Listen Port And Resource IDs Into Bounded Unsigned Types

Network config lives at a boundary where a wrong type wraps silently: a port is a
`uint16`, a resource id is a `uint32`, and the parser that reads them returns
`uint64`. You will build `ParsePort` and `ParseResourceID` so the range check runs
on the wide type and the narrowing conversion happens only after it, mirroring the
`ParseFloat(_, 32)` gotcha for unsigned integers.

## What you'll build

```text
netcfg/                     independent module: example.com/netcfg
  go.mod                    go 1.26
  netcfg.go                 ParsePort -> uint16, ParseResourceID -> uint32
  cmd/
    demo/
      main.go               parses a good and a bad port, prints both outcomes
  netcfg_test.go            valid/invalid tables, ErrRange assertion, type pin
```

Files: `netcfg.go`, `cmd/demo/main.go`, `netcfg_test.go`.
Implement: `ParsePort(raw string) (uint16, error)` and
`ParseResourceID(raw string) (uint32, error)` using `strconv.ParseUint`.
Test: valid and rejected ranges, `errors.Is(err, strconv.ErrRange)` for overflow, a
`var _ uint16 = port` type pin.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/10-type-inference-deep-dive/03-port-and-id-parsing-uint/cmd/demo
cd go-solutions/02-variables-types-and-constants/10-type-inference-deep-dive/03-port-and-id-parsing-uint
go mod edit -go=1.26
```

## Why `ParseUint(raw, 10, 16)` still returns `uint64`

`strconv.ParseUint(raw, base, bitSize)` always returns `uint64`. The `bitSize`
argument bounds the *accepted range*: `ParseUint(raw, 10, 16)` accepts `0` through
`65535` and returns `strconv.ErrRange` (wrapped in a `*strconv.NumError`) for
`65536` or larger. But the return type is `uint64` regardless — the `16` is a range
gate, not a type selector. This is the unsigned twin of `ParseFloat(raw, 32)`
returning `float64`.

So the correct shape is: parse with the right `bitSize` so out-of-range input is
rejected for free, then apply any *business* range rule the parser cannot express,
and only then convert. A TCP port is `1..65535` — `ParseUint` with `bitSize 16`
rejects `65536+`, but `0` is a valid `uint16` yet an invalid listen port, so you
reject `0` yourself. After both checks pass, `uint16(n)` is safe because you have
proven `n` fits. A resource id here is any `uint32`, so `ParseUint(raw, 10, 32)`
plus `uint32(n)` is enough; `0` is allowed.

The overflow error is worth asserting on precisely because it is a *typed* sentinel.
`errors.Is(err, strconv.ErrRange)` is true for `65536`, and false for a
non-numeric input (which yields `strconv.ErrSyntax`). Distinguishing the two lets a
caller tell "you gave me a number that is too big" from "that was not a number".

Create `netcfg.go`:

```go
package netcfg

import (
	"fmt"
	"strconv"
)

// ParsePort validates a TCP listen port. ParseUint(raw, 10, 16) returns uint64
// and rejects values above 65535 with strconv.ErrRange; port 0 is a valid
// uint16 but not a usable listen port, so it is rejected here.
func ParsePort(raw string) (uint16, error) {
	n, err := strconv.ParseUint(raw, 10, 16) // returns uint64; 16 bounds the range
	if err != nil {
		return 0, fmt.Errorf("parse port %q: %w", raw, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("port must be in 1..65535, got 0")
	}
	return uint16(n), nil // narrow only after the range is proven
}

// ParseResourceID validates a 32-bit resource id. Any uint32 is acceptable,
// including 0, so ParseUint with bitSize 32 plus a conversion is enough.
func ParseResourceID(raw string) (uint32, error) {
	n, err := strconv.ParseUint(raw, 10, 32) // returns uint64; 32 bounds the range
	if err != nil {
		return 0, fmt.Errorf("parse resource id %q: %w", raw, err)
	}
	return uint32(n), nil
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strconv"

	"example.com/netcfg"
)

func main() {
	if port, err := netcfg.ParsePort("8443"); err == nil {
		fmt.Printf("listen port: %d\n", port)
	}

	if _, err := netcfg.ParsePort("70000"); err != nil {
		fmt.Printf("rejected 70000: out-of-range=%v\n", errors.Is(err, strconv.ErrRange))
	}

	if id, err := netcfg.ParseResourceID("4294967295"); err == nil {
		fmt.Printf("resource id: %d\n", id)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
listen port: 8443
rejected 70000: out-of-range=true
resource id: 4294967295
```

## Tests

The tables cover the accepted range, the rejected boundaries, and non-numeric
input. `var _ uint16 = port` pins the concrete return type, and the overflow case
asserts `strconv.ErrRange` while the non-numeric case asserts `strconv.ErrSyntax`.

Create `netcfg_test.go`:

```go
package netcfg

import (
	"errors"
	"strconv"
	"testing"
)

func TestParsePortValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want uint16
	}{
		{"1", 1},
		{"80", 80},
		{"8443", 8443},
		{"65535", 65535},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			port, err := ParsePort(tc.raw)
			if err != nil {
				t.Fatalf("ParsePort(%q) error: %v", tc.raw, err)
			}
			var _ uint16 = port // compile-time pin on the return type
			if port != tc.want {
				t.Fatalf("ParsePort(%q) = %d, want %d", tc.raw, port, tc.want)
			}
		})
	}
}

func TestParsePortRejected(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"0", "65536", "70000", "-1", "http", ""} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if _, err := ParsePort(raw); err == nil {
				t.Fatalf("ParsePort(%q) expected error, got nil", raw)
			}
		})
	}
}

func TestParsePortOverflowIsErrRange(t *testing.T) {
	t.Parallel()

	_, err := ParsePort("65536")
	if !errors.Is(err, strconv.ErrRange) {
		t.Fatalf("ParsePort(65536) err = %v, want wrapping strconv.ErrRange", err)
	}
}

func TestParsePortNonNumericIsErrSyntax(t *testing.T) {
	t.Parallel()

	_, err := ParsePort("http")
	if !errors.Is(err, strconv.ErrSyntax) {
		t.Fatalf("ParsePort(http) err = %v, want wrapping strconv.ErrSyntax", err)
	}
}

func TestParseResourceID(t *testing.T) {
	t.Parallel()

	id, err := ParseResourceID("4294967295") // max uint32
	if err != nil {
		t.Fatalf("ParseResourceID error: %v", err)
	}
	var _ uint32 = id
	if id != 4294967295 {
		t.Fatalf("ParseResourceID = %d, want 4294967295", id)
	}
	if _, err := ParseResourceID("4294967296"); !errors.Is(err, strconv.ErrRange) {
		t.Fatalf("ParseResourceID(2^32) err = %v, want ErrRange", err)
	}
}
```

## Review

The parser is correct when the range check precedes the conversion, never the
reverse. `ParseUint`'s `bitSize` does the coarse range gate; your explicit `n == 0`
adds the business rule the parser cannot express; `uint16(n)` runs last, on a value
already proven to fit. The two sentinel assertions matter operationally: a caller
that can distinguish `ErrRange` from `ErrSyntax` can return "port out of range"
versus "port not a number" to an operator instead of a generic 400. If you ever
narrow before validating, an input like `65536` would wrap to `0` and quietly bind
a wrong port.

## Resources

- [strconv.ParseUint](https://pkg.go.dev/strconv#ParseUint) — the `uint64` return and the `bitSize` range semantics.
- [strconv errors: ErrRange and ErrSyntax](https://pkg.go.dev/strconv#pkg-variables) — the sentinels wrapped in `*strconv.NumError`.
- [Go Specification: Conversions](https://go.dev/ref/spec#Conversions) — what `uint16(n)` does when `n` is out of range.

---

Back to [02-compile-time-type-contracts.md](02-compile-time-type-contracts.md) | Next: [04-byte-size-constants-overflow.md](04-byte-size-constants-overflow.md)
