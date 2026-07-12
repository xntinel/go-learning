# Exercise 5: Numeric Overflow Guardrails Using Untyped math Constants

A metrics-ingest path accumulates `int64` counters and parses `int64` values from
config and request bodies. Both can overflow. This module uses the *untyped* `math`
package constants to bound-check values before they land in fixed-width fields, and
shows the canonical compile error that untypedness produces: `var x int64 =
math.MaxUint64` does not build.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
overflowguard/                independent module: example.com/overflowguard
  go.mod                      go 1.26
  guard.go                    SafeAddInt64, ParseBoundedInt64 using math.MaxInt64/MinInt64;
                              sentinel errors
  cmd/
    demo/
      main.go                 adds near the ceiling; parses an out-of-range string
  guard_test.go               overflow/underflow table; parse bounds; math-const usability
```

Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
Implement: `SafeAddInt64(a, b)` returning `ErrOverflow`/`ErrUnderflow` on wrap;
`ParseBoundedInt64(s)` rejecting values outside `int64` range; sentinel errors
wrapped with `%w`.
Test: `SafeAddInt64(MaxInt64, 1)` overflows; `ParseBoundedInt64` rejects a value
above `MaxInt64`; `math` constants usable in both `int` and `int64` contexts.
Verify: `go test -count=1 -race ./...`

### Why math constants are untyped

`math.MaxInt64`, `math.MinInt64`, `math.MaxInt`, and `math.MaxUint64` are untyped
constants. Untypedness is exactly why `var x int64 = math.MaxUint64` fails to
compile: `math.MaxUint64` is the constant 2^64 − 1, and materializing it into an
`int64` triggers a range check that rejects the overflow. You would use `uint64`
instead. The failing assignment is shown below as a comment, never compiled,
because its whole value is that the compiler refuses it. The upside of untypedness
is that the *same* `math.MaxInt64` bound-checks a value whether the surrounding
code is working in `int` or `int64` — no per-type constant variants.

`SafeAddInt64` guards addition against wrap. The trick is to rearrange the overflow
condition so the check itself never overflows: instead of computing `a + b` and
looking for a sign flip, test `b > 0 && a > math.MaxInt64 - b` (would overflow the
top) and `b < 0 && a < math.MinInt64 - b` (would underflow the bottom).
`math.MaxInt64 - b` is safe because `b > 0`. `ParseBoundedInt64` uses
`strconv.ParseInt(s, 10, 64)`, which already rejects values outside `int64`, and
wraps its error with a sentinel so callers can match with `errors.Is`.

Create `guard.go`:

```go
package overflowguard

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Sentinel errors, wrapped with %w so callers match them with errors.Is.
var (
	ErrOverflow  = errors.New("int64 addition overflow")
	ErrUnderflow = errors.New("int64 addition underflow")
	ErrParse     = errors.New("parse bounded int64")
)

// SafeAddInt64 returns a+b, or ErrOverflow / ErrUnderflow if the true sum does
// not fit int64. The bound checks themselves never overflow.
func SafeAddInt64(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, a, b)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("%w: %d + %d", ErrUnderflow, a, b)
	}
	return a + b, nil
}

// ParseBoundedInt64 parses a base-10 int64, rejecting anything outside the
// int64 range with an ErrParse-wrapped error.
func ParseBoundedInt64(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %v", ErrParse, s, err)
	}
	return n, nil
}

// Int64Ceiling exposes math.MaxInt64 as an int64 value for callers.
func Int64Ceiling() int64 {
	return math.MaxInt64
}

// IntCeiling exposes math.MaxInt as an int; the SAME untyped math constant kind
// works in an int context here and an int64 context above.
func IntCeiling() int {
	return math.MaxInt
}

// The line below is a compile error and is intentionally NOT built: math.MaxUint64
// is an untyped constant that does not fit int64.
//
//	var _ int64 = math.MaxUint64 // constant 18446744073709551615 overflows int64
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"math"

	"example.com/overflowguard"
)

func main() {
	if _, err := overflowguard.SafeAddInt64(math.MaxInt64, 1); err != nil {
		fmt.Println("add overflow:", errors.Is(err, overflowguard.ErrOverflow))
	}

	sum, err := overflowguard.SafeAddInt64(1000, 337)
	fmt.Printf("safe add: %d err=%v\n", sum, err)

	if _, err := overflowguard.ParseBoundedInt64("99999999999999999999"); err != nil {
		fmt.Println("parse rejected out-of-range:", errors.Is(err, overflowguard.ErrParse))
	}

	fmt.Printf("ceilings int=%d int64=%d\n", overflowguard.IntCeiling(), overflowguard.Int64Ceiling())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
add overflow: true
safe add: 1337 err=<nil>
parse rejected out-of-range: true
ceilings int=9223372036854775807 int64=9223372036854775807
```

### Tests

`TestSafeAddInt64` is table-driven over the overflow, underflow, and normal cases,
asserting the sentinel with `errors.Is`. `TestParseBoundedInt64` proves an
in-range string parses and an out-of-range one is rejected with `ErrParse`.
`TestMathConstsUsableInBothContexts` shows `math.MaxInt64` and `math.MaxInt`
landing in `int64` and `int` respectively.

Create `guard_test.go`:

```go
package overflowguard

import (
	"errors"
	"math"
	"testing"
)

func TestSafeAddInt64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		a, b    int64
		want    int64
		wantErr error
	}{
		{"normal", 1000, 337, 1337, nil},
		{"overflow at ceiling", math.MaxInt64, 1, 0, ErrOverflow},
		{"overflow large", math.MaxInt64 - 5, 10, 0, ErrOverflow},
		{"underflow at floor", math.MinInt64, -1, 0, ErrUnderflow},
		{"boundary max plus zero", math.MaxInt64, 0, math.MaxInt64, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SafeAddInt64(tt.a, tt.b)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("SafeAddInt64(%d,%d) err = %v, want %v", tt.a, tt.b, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SafeAddInt64(%d,%d) unexpected err %v", tt.a, tt.b, err)
			}
			if got != tt.want {
				t.Fatalf("SafeAddInt64(%d,%d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestParseBoundedInt64(t *testing.T) {
	t.Parallel()

	if n, err := ParseBoundedInt64("42"); err != nil || n != 42 {
		t.Fatalf("ParseBoundedInt64(42) = %d, %v", n, err)
	}
	if _, err := ParseBoundedInt64("99999999999999999999"); !errors.Is(err, ErrParse) {
		t.Fatalf("out-of-range parse err = %v, want ErrParse", err)
	}
	if _, err := ParseBoundedInt64("not-a-number"); !errors.Is(err, ErrParse) {
		t.Fatalf("non-numeric parse err = %v, want ErrParse", err)
	}
}

func TestMathConstsUsableInBothContexts(t *testing.T) {
	t.Parallel()

	var ceil64 int64 = math.MaxInt64
	var ceil int = math.MaxInt

	if Int64Ceiling() != ceil64 {
		t.Fatalf("Int64Ceiling() = %d, want %d", Int64Ceiling(), ceil64)
	}
	if IntCeiling() != ceil {
		t.Fatalf("IntCeiling() = %d, want %d", IntCeiling(), ceil)
	}
}
```

## Review

The guard is correct when the overflow checks are done with subtraction against the
`math` bound so the check never itself overflows — computing `a+b` first and
looking for a sign change is the classic wrong approach because the wrap has already
happened. The sentinel errors are wrapped with `%w`, so `errors.Is` in the tests
and in real callers works across the wrap. The lesson the commented line makes
concrete is that `math` constants are untyped and range-checked at assignment:
`math.MaxUint64` into `int64` is a build error, and the fix is `uint64`, not a
conversion that would silently truncate.

## Resources

- [math package constants](https://pkg.go.dev/math#pkg-constants) — the untyped MaxInt64/MinInt64/MaxUint64.
- [strconv.ParseInt](https://pkg.go.dev/strconv#ParseInt) — bit-size bounded parsing.
- [errors.Is and %w wrapping](https://pkg.go.dev/errors#Is) — sentinel matching across wraps.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-permission-bitmask-iota.md](04-permission-bitmask-iota.md) | Next: [06-sampler-rate-float-precision.md](06-sampler-rate-float-precision.md)
