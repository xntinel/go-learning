# Exercise 3: Narrow int64 Counters To An int32 Column Without Silent Overflow

Aggregations run in `int64` because that is the safe width for accumulation, but
the destination is often narrower: an `int32` database column, a gRPC `int32`
field, a `uint16` port. Go's `int32(x)` conversion silently wraps on overflow, so
the narrowing must be guarded by an explicit range check. This exercise builds
those guards and documents the wrap they prevent.

This module is fully self-contained: its own module, all code inline, its own
demo and tests.

## What you'll build

```text
narrow/                      independent module: example.com/narrow
  go.mod                     go 1.26
  narrow.go                  ToInt32(int64); ToUint16(int) with range checks + ErrOutOfRange
  cmd/
    demo/
      main.go                runnable demo: in-range, overflow, and raw-wrap contrast
  narrow_test.go             boundary tests + a test documenting the silent wrap
```

- Files: `narrow.go`, `cmd/demo/main.go`, `narrow_test.go`.
- Implement: `ToInt32(n int64) (int32, error)` and `ToUint16(n int) (uint16, error)`, each rejecting out-of-range values with a wrapped sentinel `ErrOutOfRange`.
- Test: boundary cases at `MaxInt32`, `MaxInt32+1`, `MinInt32`, `MinInt32-1`, mid-range; a separate test asserting `int32(int64(math.MaxInt32)+1)` is negative to justify the guard.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/narrow/cmd/demo
cd ~/go-exercises/narrow
go mod init example.com/narrow
go mod edit -go=1.26
```

### Why the compiler will not save you

A conversion `int32(n)` is defined to keep only the low 32 bits of `n` and
reinterpret them as a signed 32-bit value. There is no overflow check and no
panic. `int64(math.MaxInt32) + 1` is `2147483648`; its low 32 bits, read as
signed, are `-2147483648`. So a counter that legitimately grows past two billion
does not error when written to an `int32` column — it flips to a large negative
number, and the corruption surfaces much later as a nonsensical metric or a
constraint violation. The only defense is to compare against the target type's
bounds *before* converting.

`ToInt32` compares `n` against `math.MinInt32` and `math.MaxInt32`, which are
untyped constants exactly representable in `int64`, and returns a wrapped
`ErrOutOfRange` when the value does not fit. `ToUint16` does the same against `0`
and `math.MaxUint16` for a port-like value. The sentinel is wrapped with `%w` so
a caller can `errors.Is(err, ErrOutOfRange)` to distinguish a range failure from
any other error, and the message includes the offending value for the logs.

Create `narrow.go`:

```go
// narrow.go
package narrow

import (
	"errors"
	"fmt"
	"math"
)

// ErrOutOfRange is returned when a value does not fit the target type.
var ErrOutOfRange = errors.New("value out of range for target type")

// ToInt32 narrows an int64 to int32, rejecting values outside the int32 range
// instead of silently wrapping.
func ToInt32(n int64) (int32, error) {
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("%w: %d not in [%d, %d]", ErrOutOfRange, n, math.MinInt32, math.MaxInt32)
	}
	return int32(n), nil
}

// ToUint16 narrows an int to uint16, the width of a TCP port field, rejecting
// negatives and values above 65535.
func ToUint16(n int) (uint16, error) {
	if n < 0 || n > math.MaxUint16 {
		return 0, fmt.Errorf("%w: %d not in [0, %d]", ErrOutOfRange, n, math.MaxUint16)
	}
	return uint16(n), nil
}
```

### The runnable demo

The demo narrows an in-range counter, rejects an overflowing one, and prints the
raw conversion's wrap so the danger is visible.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"math"

	"example.com/narrow"
)

func main() {
	if v, err := narrow.ToInt32(2_000_000_000); err == nil {
		fmt.Printf("in range: %d\n", v)
	}

	if _, err := narrow.ToInt32(int64(math.MaxInt32) + 1); err != nil {
		fmt.Printf("guarded:  %v\n", err)
	}

	overflow := int64(math.MaxInt32) + 1 // runtime value, not a constant
	raw := int32(overflow)               // no guard: silent wrap
	fmt.Printf("raw wrap: %d\n", raw)

	if p, err := narrow.ToUint16(8080); err == nil {
		fmt.Printf("port:     %d\n", p)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in range: 2000000000
guarded:  value out of range for target type: 2147483648 not in [-2147483648, 2147483647]
raw wrap: -2147483648
port:     8080
```

### Tests

The boundary table checks the value on both sides of each limit. A separate test
documents the exact wrap the guard prevents, so the reason the guard exists is
itself under test.

Create `narrow_test.go`:

```go
// narrow_test.go
package narrow

import (
	"errors"
	"math"
	"testing"
)

func TestToInt32(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      int64
		want    int32
		wantErr bool
	}{
		{"zero", 0, 0, false},
		{"mid range", 1_000_000, 1_000_000, false},
		{"max ok", math.MaxInt32, math.MaxInt32, false},
		{"max plus one", int64(math.MaxInt32) + 1, 0, true},
		{"min ok", math.MinInt32, math.MinInt32, false},
		{"min minus one", int64(math.MinInt32) - 1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ToInt32(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrOutOfRange) {
					t.Fatalf("ToInt32(%d) err = %v, want ErrOutOfRange", tt.in, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ToInt32(%d) = %d,%v; want %d,nil", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestToUint16(t *testing.T) {
	t.Parallel()
	if _, err := ToUint16(-1); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("ToUint16(-1) should reject, got %v", err)
	}
	if _, err := ToUint16(70000); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("ToUint16(70000) should reject, got %v", err)
	}
	if v, err := ToUint16(443); err != nil || v != 443 {
		t.Fatalf("ToUint16(443) = %d,%v; want 443,nil", v, err)
	}
}

// TestRawConversionWraps documents the silent overflow the guard prevents.
func TestRawConversionWraps(t *testing.T) {
	t.Parallel()
	overflow := int64(math.MaxInt32) + 1 // runtime value, not a constant
	raw := int32(overflow)
	if raw >= 0 {
		t.Fatalf("expected the raw conversion to wrap negative, got %d", raw)
	}
}
```

## Review

The guards are correct when every value strictly outside `[MinInt32, MaxInt32]`
(or `[0, MaxUint16]`) returns `ErrOutOfRange` and every value inside converts
unchanged. The comparison must use the target width's real bounds, taken from
`math`, not a hand-typed literal that is easy to get wrong by a digit. The
`TestRawConversionWraps` case is deliberately not testing your code — it tests
the language, pinning the fact that `int32(MaxInt32+1)` is negative so the reason
the guard is mandatory stays documented. The same discipline applies to any
narrowing: `int64`→`int` (platform-dependent width, see Exercise 1), `int`→`int8`
for a small enum, or `uint64`→`uint32` for a hash bucket.

## Resources

- [Go Specification: Conversions](https://go.dev/ref/spec#Conversions) — the defined truncation behavior of numeric conversions.
- [math constants](https://pkg.go.dev/math#pkg-constants) — `MinInt32`, `MaxInt32`, `MaxUint16`, and the rest of the width bounds.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching the wrapped `ErrOutOfRange` sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-sql-scanner-valuer-money-type.md](04-sql-scanner-valuer-money-type.md)
