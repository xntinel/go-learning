# Exercise 3: Differential Fuzz A Hand-Rolled IPv4 Parser Against netip

A request logger on the hot path parses the client IP out of every request. The
allocation-free stdlib `net/netip` is usually enough, but suppose profiling
pushed you to a hand-rolled dotted-quad parser that returns a fixed `[4]byte` and
never allocates. A hand-rolled parser is exactly where subtle divergences hide.
This module fuzzes it *differentially* against `net/netip` as the trusted oracle:
whenever the two disagree, the fuzzer minimizes the disagreement into a tiny
reproducer.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
ipv4/                      independent module: example.com/ipv4
  go.mod                   module path
  parse.go                 ParseIPv4(string) ([4]byte, bool) — strict, allocation-free
  cmd/
    demo/
      main.go              parse a few addresses, print accept/reject and bytes
  parse_test.go            TestParseIPv4Table, FuzzParseIPv4 (vs netip), Example
```

Files: `parse.go`, `cmd/demo/main.go`, `parse_test.go`.
Implement: `ParseIPv4(s string) ([4]byte, bool)` accepting exactly the strict
dotted-quad form (four octets, `0`-`255`, no leading zeros, no spaces).
Test: a table test for the known edge cases; `FuzzParseIPv4` asserting agreement
with `net/netip` on both value and reject decision.
Verify: `go test -race ./...`, then `go test -fuzz=FuzzParseIPv4 -fuzztime=2s`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/06-fuzz-testing/03-differential-ipv4-parser/cmd/demo
cd go-solutions/12-testing-ecosystem/06-fuzz-testing/03-differential-ipv4-parser
```

### Why differential fuzzing, and what "agree" means precisely

The property is not "the parser is correct in the abstract" — it is "the parser
agrees with `net/netip`". That is a sharper, checkable claim, and it is the right
one because `netip` is the reference every other tool in the ecosystem trusts.
The subtlety is defining agreement so the test does not produce false failures.

`netip.ParseAddr` accepts far more than dotted quads: `"::1"` is a valid IPv6
address, and `"::ffff:1.2.3.4"` is an IPv4-mapped IPv6 address. Neither is a
dotted quad, so the custom parser must reject both — but `netip` *accepts* them.
The reconciling predicate is `Is4()`: an address is a genuine IPv4 dotted quad
exactly when `netip.ParseAddr` succeeds *and* the resulting `Addr.Is4()` is true.
So the property is:

- If `netip.ParseAddr(s)` succeeds and `addr.Is4()`, then `ParseIPv4(s)` must
  return `(addr.As4(), true)` — same four bytes.
- Otherwise (parse error, or a non-`Is4` address like IPv6), `ParseIPv4(s)` must
  return `ok == false`.

Calling `addr.As4()` on a non-IPv4 address *panics*, so the test must gate on
`Is4()` before it — the normalization the Common Mistakes warns about. The custom
parser must match `netip`'s strictness exactly, and `netip` is strict: it rejects
leading zeros (`"01.2.3.4"`), out-of-range octets (`"256.0.0.0"`), the wrong
field count (`"1.2.3"`), and surrounding spaces. Encoding that strictness by hand
is precisely the code a fuzzer stresses.

Create `parse.go`:

```go
package ipv4

// ParseIPv4 parses a strict dotted-quad IPv4 address into four bytes. It accepts
// exactly four decimal octets in [0,255] separated by dots, with no leading
// zeros (except a lone "0"), no spaces, and no other forms. It allocates
// nothing. ok is false for any input outside that grammar.
func ParseIPv4(s string) (out [4]byte, ok bool) {
	field := 0  // which octet, 0..3
	val := 0    // accumulated value of the current octet
	digits := 0 // digits seen in the current octet
	leadZero := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if digits == 1 && leadZero {
				return out, false // "01" style leading zero
			}
			if digits == 0 && c == '0' {
				leadZero = true
			}
			val = val*10 + int(c-'0')
			digits++
			if digits > 3 || val > 255 {
				return out, false
			}
		case c == '.':
			if digits == 0 || field == 3 {
				return out, false // empty octet or too many dots
			}
			out[field] = byte(val)
			field++
			val, digits, leadZero = 0, 0, false
		default:
			return out, false // any other byte
		}
	}
	if field != 3 || digits == 0 {
		return out, false // not exactly four octets
	}
	out[3] = byte(val)
	return out, true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ipv4"
)

func main() {
	for _, s := range []string{"192.168.1.1", "255.255.255.255", "01.2.3.4", "256.0.0.1", "::1"} {
		b, ok := ipv4.ParseIPv4(s)
		if ok {
			fmt.Printf("%-18s accept %v\n", s, b)
		} else {
			fmt.Printf("%-18s reject\n", s)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
192.168.1.1        accept [192 168 1 1]
255.255.255.255    accept [255 255 255 255]
01.2.3.4           reject
256.0.0.1          reject
::1                reject
```

### Tests

The table test pins the exact edge cases in the prose. `FuzzParseIPv4` runs the
differential check: it normalizes `netip`'s answer to the "genuine dotted quad"
predicate and asserts the custom parser matches it on every generated string.

Create `parse_test.go`:

```go
package ipv4

import (
	"fmt"
	"net/netip"
	"testing"
)

func TestParseIPv4Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   [4]byte
		accept bool
	}{
		{"0.0.0.0", [4]byte{0, 0, 0, 0}, true},
		{"1.2.3.4", [4]byte{1, 2, 3, 4}, true},
		{"255.255.255.255", [4]byte{255, 255, 255, 255}, true},
		{"01.2.3.4", [4]byte{}, false},
		{"256.1.1.1", [4]byte{}, false},
		{"1.2.3", [4]byte{}, false},
		{"1.2.3.4.5", [4]byte{}, false},
		{" 1.2.3.4", [4]byte{}, false},
		{"1.2.3.", [4]byte{}, false},
		{"::1", [4]byte{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseIPv4(tc.in)
			if ok != tc.accept {
				t.Fatalf("ParseIPv4(%q) ok = %v, want %v", tc.in, ok, tc.accept)
			}
			if ok && got != tc.want {
				t.Fatalf("ParseIPv4(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func FuzzParseIPv4(f *testing.F) {
	for _, s := range []string{"1.2.3.4", "0.0.0.0", "255.255.255.255", "01.2.3.4", "256.1.1.1", "1.2.3", "::1", "::ffff:1.2.3.4", " 1.2.3.4"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, ok := ParseIPv4(s)

		addr, err := netip.ParseAddr(s)
		isDottedQuad := err == nil && addr.Is4()

		if ok != isDottedQuad {
			t.Fatalf("ParseIPv4(%q) accepted=%v, but netip dotted-quad=%v", s, ok, isDottedQuad)
		}
		if isDottedQuad && got != addr.As4() {
			t.Fatalf("ParseIPv4(%q) = %v, netip = %v", s, got, addr.As4())
		}
	})
}

func Example() {
	b, ok := ParseIPv4("10.0.0.7")
	fmt.Println(b, ok)
	// Output: [10 0 0 7] true
}
```

## Review

The parser is correct when it agrees with `net/netip` for every input the fuzzer
throws at it — same bytes on accept, same reject decision on everything else. The
two things that make the differential test *honest* rather than flaky are both in
the fuzz body: gating on `Is4()` so `As4()` is never called on an IPv6 address
(which would panic and mask the real property), and defining "accept" as `netip`
accepts *and* `Is4()` so an IPv4-in-IPv6 form is a reject for both sides. If the
hand-rolled strictness drifts — a forgotten leading-zero check, an octet allowed
to reach 256 — the fuzzer minimizes the divergence to a string like `"010.0.0.0"`
that you paste straight into the table test. Run `go test -race ./...`, then
`go test -fuzz=FuzzParseIPv4 -fuzztime=2s`.

## Resources

- [`net/netip.ParseAddr`](https://pkg.go.dev/net/netip#ParseAddr) — the trusted oracle, with `Addr.Is4` and `Addr.As4`.
- [`net/netip` overview](https://pkg.go.dev/net/netip) — why the compact `Addr` type is allocation-free and how IPv4-in-IPv6 forms work.
- [Go Fuzzing reference](https://go.dev/doc/security/fuzz/) — minimization turns a divergence into a one-line reproducer.

---

Back to [02-roundtrip-uvarint-codec.md](02-roundtrip-uvarint-codec.md) | Next: [04-no-panic-header-parser.md](04-no-panic-header-parser.md)
