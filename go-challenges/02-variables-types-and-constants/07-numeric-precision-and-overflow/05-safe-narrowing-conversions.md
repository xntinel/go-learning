# Exercise 5: Range-Checked Integer Narrowing at Config and RPC Boundaries

A config loader parses `PORT=70000` from the environment as an `int64`, then a
downstream call needs it as a `uint16`. Writing `uint16(70000)` compiles, runs, and
silently produces `4464` — a different, valid-looking port. This exercise builds the
guarded narrowing helpers that a config or RPC boundary needs: convert wide integers
into the narrow types downstream systems demand, returning an error instead of a
truncated value.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
narrowing/                   independent module: example.com/narrowing
  go.mod                     module path
  narrowing.go               ToUint16, ToInt32, ToInt; ParsePort, ParsePageSize; ErrRange
  cmd/
    demo/
      main.go                loads a port and page size, shows truncation prevented
  narrowing_test.go          boundary table per target, naive int32(n) contrast
```

Files: `narrowing.go`, `cmd/demo/main.go`, `narrowing_test.go`.
Implement: guarded helpers `ToUint16(int64) (uint16, error)`, `ToInt32(int64)
(int32, error)`, `ToInt(int64) (int, error)`, each checking the destination bounds
before converting; plus `ParsePort(string) (uint16, error)` and `ParsePageSize(string)
(int32, error)` that combine `strconv` parsing with the guard.
Test: boundary values per target (65535 ok / 65536 rejected; `math.MaxInt32` ok /
`+1` rejected; negatives rejected for unsigned); assert the in-range value is
returned unchanged and contrast against a naive `int32(n)` that truncates.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/07-numeric-precision-and-overflow/05-safe-narrowing-conversions/cmd/demo
cd go-solutions/02-variables-types-and-constants/07-numeric-precision-and-overflow/05-safe-narrowing-conversions
```

### Why an explicit conversion is not a safe conversion

Go forces you to *write* a conversion when narrowing — the compiler will not let an
`int64` flow into a `uint16` implicitly — and that visibility fools people into
thinking the conversion is checked. It is not. `uint16(n)` is defined to keep the low
16 bits of `n`; `int32(n)` keeps the low 32; `int(n)` on a 32-bit build keeps the low
32 of a 64-bit value. When `n` is outside the destination range, the result is a
truncated bit pattern, delivered with no error and no panic. A port becomes a
different port, a record ID becomes a different ID, a length becomes a nonsense slice
size. The corruption is invisible until the first out-of-range value arrives in
production.

The fix is a guard that compares against the destination's limits *before*
converting. `ToUint16` rejects `n < 0` (unsigned targets have no negatives) and `n >
math.MaxUint16`, and only then returns `uint16(n)`, which is now provably lossless.
`ToInt32` checks both `math.MinInt32` and `math.MaxInt32`, because a signed target can
overflow in either direction. `ToInt` guards against `math.MinInt` / `math.MaxInt`;
on a 64-bit platform an `int64` always fits so the check is a no-op, but writing it
keeps the code correct on a 32-bit build where it does not — the guard documents and
enforces the assumption rather than leaving it implicit. `ParsePort` and
`ParsePageSize` compose `strconv.ParseInt` (which rejects non-numeric text) with the
narrowing guard, so a single call turns untrusted text into a bounded narrow value or
a descriptive error.

Create `narrowing.go`:

```go
package narrowing

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// ErrRange is returned when a value does not fit the destination integer type.
var ErrRange = errors.New("value out of range for target type")

// ToUint16 narrows an int64 to uint16, or returns ErrRange instead of truncating.
func ToUint16(n int64) (uint16, error) {
	if n < 0 || n > math.MaxUint16 {
		return 0, fmt.Errorf("%d does not fit uint16 [0,%d]: %w", n, math.MaxUint16, ErrRange)
	}
	return uint16(n), nil
}

// ToInt32 narrows an int64 to int32 with a both-direction bounds check.
func ToInt32(n int64) (int32, error) {
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("%d does not fit int32 [%d,%d]: %w", n, math.MinInt32, math.MaxInt32, ErrRange)
	}
	return int32(n), nil
}

// ToInt narrows an int64 to int. On a 64-bit platform the range is identical, but
// the guard keeps the code correct on a 32-bit build.
func ToInt(n int64) (int, error) {
	if n < math.MinInt || n > math.MaxInt {
		return 0, fmt.Errorf("%d does not fit int: %w", n, ErrRange)
	}
	return int(n), nil
}

// ParsePort parses a TCP port from text and narrows it to uint16.
func ParsePort(s string) (uint16, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse port %q: %w", s, ErrRange)
	}
	return ToUint16(n)
}

// ParsePageSize parses a page size from text and narrows it to int32.
func ParsePageSize(s string) (int32, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse page size %q: %w", s, ErrRange)
	}
	return ToInt32(n)
}
```

### The runnable demo

The demo loads a valid port and page size, then shows what the guard prevents by
printing both the guarded result and the truncated value a naive conversion would
have produced.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/narrowing"
)

func main() {
	port, err := narrowing.ParsePort("8443")
	if err != nil {
		fmt.Println("port:", err)
		return
	}
	fmt.Printf("port=%d\n", port)

	tooBig := int64(70000) // a runtime value, not a constant
	if _, err := narrowing.ToUint16(tooBig); err != nil {
		fmt.Printf("guarded: %v\n", err)
		fmt.Printf("naive uint16(%d) would have been %d\n", tooBig, uint16(tooBig))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
port=8443
guarded: 70000 does not fit uint16 [0,65535]: value out of range for target type
naive uint16(70000) would have been 4464
```

### Tests

The tests walk each target's boundary. For the port: `65535` succeeds and returns the
input unchanged, `65536` is rejected, and a negative is rejected. For `int32`:
`math.MaxInt32` succeeds and `math.MaxInt32 + 1` is rejected. The contrast case makes
the danger concrete: for a value one past the limit, the guard returns `ErrRange`
while a raw `int32(n)` wraps to a negative number — the truncation the guard exists
to prevent.

Create `narrowing_test.go`:

```go
package narrowing

import (
	"errors"
	"math"
	"testing"
)

func TestToUint16Boundaries(t *testing.T) {
	t.Parallel()

	if got, err := ToUint16(65535); err != nil || got != 65535 {
		t.Fatalf("ToUint16(65535) = %d,%v; want 65535,nil", got, err)
	}
	if _, err := ToUint16(65536); !errors.Is(err, ErrRange) {
		t.Fatalf("ToUint16(65536) error = %v, want ErrRange", err)
	}
	if _, err := ToUint16(-1); !errors.Is(err, ErrRange) {
		t.Fatalf("ToUint16(-1) error = %v, want ErrRange", err)
	}
}

func TestToInt32Boundaries(t *testing.T) {
	t.Parallel()

	if got, err := ToInt32(math.MaxInt32); err != nil || int64(got) != math.MaxInt32 {
		t.Fatalf("ToInt32(MaxInt32) = %d,%v; want MaxInt32,nil", got, err)
	}
	if _, err := ToInt32(math.MaxInt32 + 1); !errors.Is(err, ErrRange) {
		t.Fatalf("ToInt32(MaxInt32+1) error = %v, want ErrRange", err)
	}
	if got, err := ToInt32(math.MinInt32); err != nil || int64(got) != math.MinInt32 {
		t.Fatalf("ToInt32(MinInt32) = %d,%v; want MinInt32,nil", got, err)
	}
}

func TestGuardPreventsTruncation(t *testing.T) {
	t.Parallel()

	n := int64(math.MaxInt32) + 1 // runtime value: the conversion truncates, not the compiler
	if _, err := ToInt32(n); !errors.Is(err, ErrRange) {
		t.Fatalf("ToInt32(%d) error = %v, want ErrRange", n, err)
	}
	// The naive conversion the guard replaces truncates to a negative value.
	if int32(n) != math.MinInt32 {
		t.Fatalf("naive int32(%d) = %d, expected wrap to MinInt32", n, int32(n))
	}
}

func TestParseHelpers(t *testing.T) {
	t.Parallel()

	if p, err := ParsePort("8443"); err != nil || p != 8443 {
		t.Fatalf("ParsePort(8443) = %d,%v; want 8443,nil", p, err)
	}
	if _, err := ParsePort("70000"); !errors.Is(err, ErrRange) {
		t.Fatalf("ParsePort(70000) error = %v, want ErrRange", err)
	}
	if _, err := ParsePort("notaport"); !errors.Is(err, ErrRange) {
		t.Fatalf("ParsePort(notaport) error = %v, want ErrRange", err)
	}
	if sz, err := ParsePageSize("100"); err != nil || sz != 100 {
		t.Fatalf("ParsePageSize(100) = %d,%v; want 100,nil", sz, err)
	}
}
```

## Review

The helpers are correct when an in-range value passes through unchanged and every
out-of-range value returns `ErrRange` rather than a wrapped result. The load-bearing
test is `TestGuardPreventsTruncation`: it pins the exact failure the guard prevents by
showing that the same input a guarded call rejects, a raw `int32(n)` silently wraps to
`math.MinInt32`. Two traps to avoid: forgetting the lower bound on a signed target (a
large negative `int64` overflows `int32` downward just as a large positive one
overflows upward), and forgetting that unsigned targets reject negatives outright. The
`ToInt` no-op guard on a 64-bit platform is deliberate — it keeps the boundary honest
where `int` is only 32 bits wide.

## Resources

- [Go Specification: Conversions](https://go.dev/ref/spec#Conversions) — the defined truncation behavior of a narrowing integer conversion.
- [`math` constants](https://pkg.go.dev/math#pkg-constants) — `MaxUint16`, `MaxInt32`, `MinInt32`, `MaxInt`, `MinInt`.
- [`strconv.ParseInt`](https://pkg.go.dev/strconv#ParseInt) — base-10, 64-bit parsing the helpers narrow from.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-exact-tax-and-percentage-big-rat.md](06-exact-tax-and-percentage-big-rat.md)
