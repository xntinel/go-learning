# Exercise 4: Byte-Size Limit Constants: Arbitrary Precision vs Fixed-Width Fields

Size limits are where untyped constants and fixed-width fields collide. You will
build a `limits` package with the standard binary size units and a `Limits` struct
that mixes `int64` and `int32` fields, and see exactly where the compiler protects
you (an overflowing constant assignment refuses to build) and where it does not
(a `:=` that infers a 64-bit `int` on a 64-bit host).

## What you'll build

```text
sizelimits/                 independent module: example.com/sizelimits
  go.mod                    go 1.26
  limits.go                 KiB..TiB consts, type Limits, HumanBytes, ParseSize
  cmd/
    demo/
      main.go               prints the unit values, platform int width, a limit
  limits_test.go            value assertions, HumanBytes/ParseSize round-trip
```

Files: `limits.go`, `cmd/demo/main.go`, `limits_test.go`.
Implement: `KiB..TiB` untyped constants, a `Limits` struct, `HumanBytes(n int64)`
and `ParseSize(raw string)`.
Test: numeric values of the constants, a `HumanBytes`/`ParseSize` round-trip, a
`var _ int64` pin, and a documented does-not-compile overflow.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

## Arbitrary precision, until a fixed-width field says otherwise

`KiB = 1 << 10`, `MiB = 1 << 20`, `GiB = 1 << 30`, `TiB = 1 << 40` are untyped
constants. Untyped constants are evaluated at *arbitrary precision*, so
`const maxUpload = 5 << 30` (5 GiB) is a legal constant even though the value is
large, and `5 * TiB` is a legal constant even though it far exceeds what an `int32`
could hold. Nothing overflows while the value lives as a constant expression.

The overflow becomes a *compile-time error* the instant the constant is assigned to
a fixed-width target that cannot hold it. `var maxUpload int64 = 5 << 30` is fine
(5 GiB fits in `int64`). `var tiny int32 = 1 << 40` does not compile — the compiler
reports "constant 1099511627776 overflows int32" at the assignment site, before any
test, before any deploy. That is a guardrail: a size field typed `int32` will
*refuse* to be initialized with a constant it cannot represent. The `Limits` struct
below mixes an `int64 MaxUpload` (needs to hold gigabytes) with an
`int32 MaxHeader` (kilobytes are plenty), and only the correctly-sized assignments
compile.

The trap the compiler does *not* catch is inference width. `x := 1 << 40` compiles
and infers `int`, which is 64-bit on the amd64/arm64 hosts servers run on — so it
holds 1 TiB fine. But anyone who assumes `int` is 32 bits (a habit from C on some
platforms, or from 32-bit builds) is carrying an operational misconception:
`int` is platform-width, and truncation from `int` would only appear on a 32-bit
target. The demo prints `unsafe.Sizeof(int(0))` so the width is visible, not
assumed.

Create `limits.go`:

```go
package sizelimits

import (
	"fmt"
	"strconv"
	"strings"
)

// Binary size units as untyped constants. They have arbitrary precision as
// constants and only take a type when assigned to a typed target.
const (
	KiB = 1 << 10
	MiB = 1 << 20
	GiB = 1 << 30
	TiB = 1 << 40
)

// maxUpload is a typed constant that fits int64. The struct field type below is
// what makes this assignment legal; the same value into an int32 would not
// compile.
const maxUpload int64 = 5 << 30 // 5 GiB

// Compile-time pin: maxUpload must stay an int64-representable size.
var _ int64 = maxUpload

// The following assignment does not compile, and that is the point:
//
//	var tooBig int32 = 1 << 40 // constant 1099511627776 overflows int32
//
// An int32 size field refuses a constant it cannot hold, at build time.

// Limits mixes a 64-bit and a 32-bit field on purpose: MaxUpload must hold
// gigabytes (int64), MaxHeader only ever holds kilobytes (int32 is plenty).
type Limits struct {
	MaxUpload int64 // bytes, up to many GiB
	MaxHeader int32 // bytes, small by design
}

// DefaultLimits returns sane defaults. Each untyped constant adopts the field
// type it initializes.
func DefaultLimits() Limits {
	return Limits{
		MaxUpload: maxUpload, // 5 GiB, fits int64
		MaxHeader: 1 * MiB,   // 1 MiB header cap, fits int32
	}
}

// HumanBytes renders a byte count using the largest binary unit that divides it
// evenly, falling back to a plain byte count.
func HumanBytes(n int64) string {
	switch {
	case n >= TiB && n%TiB == 0:
		return fmt.Sprintf("%dTiB", n/TiB)
	case n >= GiB && n%GiB == 0:
		return fmt.Sprintf("%dGiB", n/GiB)
	case n >= MiB && n%MiB == 0:
		return fmt.Sprintf("%dMiB", n/MiB)
	case n >= KiB && n%KiB == 0:
		return fmt.Sprintf("%dKiB", n/KiB)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// ParseSize parses a value like "5GiB" or "2048" into a byte count. The digits
// are parsed with ParseInt (returns int64) and multiplied by the unit factor.
func ParseSize(raw string) (int64, error) {
	units := []struct {
		suffix string
		factor int64
	}{
		{"TiB", TiB},
		{"GiB", GiB},
		{"MiB", MiB},
		{"KiB", KiB},
	}
	for _, u := range units {
		if strings.HasSuffix(raw, u.suffix) {
			digits := strings.TrimSuffix(raw, u.suffix)
			n, err := strconv.ParseInt(digits, 10, 64) // returns int64
			if err != nil {
				return 0, fmt.Errorf("parse size %q: %w", raw, err)
			}
			return n * u.factor, nil
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", raw, err)
	}
	return n, nil
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unsafe"

	"example.com/sizelimits"
)

func main() {
	fmt.Printf("KiB=%d MiB=%d GiB=%d TiB=%d\n",
		sizelimits.KiB, sizelimits.MiB, sizelimits.GiB, sizelimits.TiB)

	fmt.Printf("platform int is %d bytes\n", unsafe.Sizeof(int(0)))

	lim := sizelimits.DefaultLimits()
	fmt.Printf("max upload: %s\n", sizelimits.HumanBytes(lim.MaxUpload))

	n, _ := sizelimits.ParseSize("2GiB")
	fmt.Printf("parsed 2GiB = %d bytes = %s\n", n, sizelimits.HumanBytes(n))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on a 64-bit host):

```
KiB=1024 MiB=1048576 GiB=1073741824 TiB=1099511627776
platform int is 8 bytes
max upload: 5GiB
parsed 2GiB = 2147483648 bytes = 2GiB
```

## Tests

The tests assert the concrete numeric values of the unit constants (so a typo in a
shift shows up), pin `MaxUpload` to `int64`, and round-trip `HumanBytes` against
`ParseSize`.

Create `limits_test.go`:

```go
package sizelimits

import (
	"errors"
	"strconv"
	"testing"
)

func TestUnitValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  int64
		want int64
	}{
		{"KiB", KiB, 1024},
		{"MiB", MiB, 1048576},
		{"GiB", GiB, 1073741824},
		{"TiB", TiB, 1099511627776},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestDefaultLimitsFieldTypes(t *testing.T) {
	t.Parallel()

	lim := DefaultLimits()
	var _ int64 = lim.MaxUpload // pin
	var _ int32 = lim.MaxHeader // pin
	if lim.MaxUpload != 5*GiB {
		t.Fatalf("MaxUpload = %d, want %d", lim.MaxUpload, int64(5*GiB))
	}
	if lim.MaxHeader != 1*MiB {
		t.Fatalf("MaxHeader = %d, want %d", lim.MaxHeader, int32(1*MiB))
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		n    int64
		want string
	}{
		{5 * GiB, "5GiB"},
		{2 * MiB, "2MiB"},
		{1 * TiB, "1TiB"},
		{512, "512B"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.n); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestParseSizeRoundTrip(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"5GiB", "2MiB", "1TiB", "2048"} {
		n, err := ParseSize(raw)
		if err != nil {
			t.Fatalf("ParseSize(%q) error: %v", raw, err)
		}
		var _ int64 = n
		if n <= 0 {
			t.Fatalf("ParseSize(%q) = %d, want positive", raw, n)
		}
	}
}

func TestParseSizeRejectsGarbage(t *testing.T) {
	t.Parallel()

	_, err := ParseSize("bigGiB")
	if !errors.Is(err, strconv.ErrSyntax) {
		t.Fatalf("ParseSize(bigGiB) err = %v, want wrapping strconv.ErrSyntax", err)
	}
}
```

## Review

The lesson is in the two behaviors side by side. Assigning `1 << 40` to an `int32`
field is a *build error* you can rely on: a size field typed too narrow for its
constant will not compile, so you cannot ship a truncated limit. But `x := 1 << 40`
compiles and infers `int` — 64-bit on servers, and the source of a truncation only
if someone builds for a 32-bit target. Keep size constants untyped so they adapt to
each field, let the field type be the guardrail, and never assume `int` is any
particular width — print `unsafe.Sizeof` when it matters. `HumanBytes` and
`ParseSize` both keep byte counts in `int64`, the only sane storage width for sizes
that reach gigabytes.

## Resources

- [The Go Blog: Constants](https://go.dev/blog/constants) — arbitrary precision and default types of untyped constants.
- [Go Specification: Constant expressions](https://go.dev/ref/spec#Constant_expressions) — when a constant overflows its target.
- [Go Specification: Numeric types](https://go.dev/ref/spec#Numeric_types) — `int` is platform-width; `int32`/`int64` are fixed.

---

Back to [03-port-and-id-parsing-uint.md](03-port-and-id-parsing-uint.md) | Next: [05-log-level-iota-enum.md](05-log-level-iota-enum.md)
